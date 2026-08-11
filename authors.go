package arxiv

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ParseAuthors splits an author string into individual author names.
func ParseAuthors(authors string) []string {
	if strings.Contains(authors, " and ") {
		return splitNonEmpty(authors, " and ")
	}
	parts := splitNonEmpty(authors, ",")
	if len(parts) >= 2 && len(parts)%2 == 0 {
		commaNames := true
		for _, part := range parts {
			if strings.ContainsAny(part, " \t") {
				commaNames = false
				break
			}
		}
		if commaNames {
			result := make([]string, 0, len(parts)/2)
			for i := 0; i < len(parts); i += 2 {
				result = append(result, parts[i]+", "+parts[i+1])
			}
			return result
		}
	}
	return parts
}

func splitNonEmpty(value, separator string) []string {
	var result []string
	for _, a := range strings.Split(value, separator) {
		a = strings.TrimSpace(a)
		if a != "" {
			result = append(result, a)
		}
	}
	return result
}

// normalizeAuthor normalizes an author name for consistent matching.
func normalizeAuthor(name string) string {
	return strings.TrimSpace(name)
}

// CollaboratorInfo contains information about a collaborator.
type CollaboratorInfo struct {
	Author      string    `json:"author"`
	PaperCount  int       `json:"paper_count"`
	PaperIDs    []string  `json:"paper_ids"`
	FirstCollab time.Time `json:"first_collab"`
	LastCollab  time.Time `json:"last_collab"`
}

// SimilarAuthor contains information about a similar author.
type SimilarAuthor struct {
	Author     string  `json:"author"`
	Similarity float64 `json:"similarity"`
	PaperCount int     `json:"paper_count"`
}

// flipAuthorName converts between "First Last" and "Last, First" formats.
// Returns the flipped version, or empty string if it can't be flipped.
func flipAuthorName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}

	// Check if it's "Last, First" format
	if idx := strings.Index(name, ", "); idx > 0 {
		last := name[:idx]
		first := name[idx+2:]
		return first + " " + last
	}

	// It's "First Last" format - find the last space
	if idx := strings.LastIndex(name, " "); idx > 0 {
		first := name[:idx]
		last := name[idx+1:]
		return last + ", " + first
	}

	return ""
}

// normalizeToFirstLast converts "Last, First" to "First Last" format.
func normalizeToFirstLast(name string) string {
	name = strings.TrimSpace(name)
	if idx := strings.Index(name, ", "); idx > 0 {
		last := name[:idx]
		first := name[idx+2:]
		return first + " " + last
	}
	return name
}

// GetCollaborators returns collaborators for an author sorted by paper count.
// Computes directly from papers table for real-time accuracy.
func (c *Cache) GetCollaborators(ctx context.Context, author string, limit int) ([]CollaboratorInfo, error) {
	if limit <= 0 {
		limit = 20
	}
	cacheKey := detailKey("author_collaborators", author, fmt.Sprint(limit))
	if cached, ok := c.getDetailCache(cacheKey); ok {
		if collabs, ok := cached.([]CollaboratorInfo); ok {
			return cloneCollaborators(collabs), nil
		}
	}

	value, err, _ := c.detailFlights.Do(cacheKey, func() (interface{}, error) {
		if cached, ok := c.getDetailCache(cacheKey); ok {
			if collabs, ok := cached.([]CollaboratorInfo); ok {
				return cloneCollaborators(collabs), nil
			}
		}
		collabs, err := c.getCollaboratorsUncached(ctx, author, limit)
		if err != nil {
			return nil, err
		}
		c.putDetailCache(cacheKey, authorProfileTTL, cloneCollaborators(collabs))
		return collabs, nil
	})
	if err != nil {
		return nil, err
	}
	collabs, _ := value.([]CollaboratorInfo)
	return cloneCollaborators(collabs), nil
}

func (c *Cache) getCollaboratorsUncached(ctx context.Context, author string, limit int) ([]CollaboratorInfo, error) {
	// Compute collaborators directly from papers
	likeOp := "LIKE"
	if c.dbType == DBTypePostgres {
		likeOp = "ILIKE"
	}

	// Search for both "First Last" and "Last, First" formats
	flipped := flipAuthorName(author)

	// Get all papers by this author (try both name formats)
	var papers []Paper
	var err error
	err = c.withAuthorQuery(ctx, func() error {
		if flipped != "" {
			return c.db.WithContext(ctx).
				Where("authors "+likeOp+" ? OR authors "+likeOp+" ?", "%"+author+"%", "%"+flipped+"%").
				Select("id", "authors", "created").
				Find(&papers).Error
		}
		return c.db.WithContext(ctx).
			Where("authors "+likeOp+" ?", "%"+author+"%").
			Select("id", "authors", "created").
			Find(&papers).Error
	})
	if err != nil {
		return nil, fmt.Errorf("get author papers: %w", err)
	}

	// Track seen papers to avoid duplicates from OR query
	seenPapers := make(map[string]bool)

	// Count co-authors across all papers
	collabMap := make(map[string]*CollaboratorInfo)
	authorNorm := normalizeToFirstLast(author)
	authorFlipped := flipAuthorName(author)

	for _, paper := range papers {
		// Skip if we've already processed this paper (dedup from OR query)
		if seenPapers[paper.ID] {
			continue
		}
		seenPapers[paper.ID] = true

		coauthors := ParseAuthors(paper.Authors)
		for _, coauthor := range coauthors {
			// Normalize coauthor to "First Last" format for consistent keys
			coauthorNorm := normalizeToFirstLast(coauthor)

			// Skip self (check both formats)
			if strings.EqualFold(coauthorNorm, authorNorm) ||
				strings.EqualFold(coauthor, author) ||
				(authorFlipped != "" && strings.EqualFold(coauthor, authorFlipped)) {
				continue
			}

			if existing, ok := collabMap[coauthorNorm]; ok {
				existing.PaperCount++
				existing.PaperIDs = append(existing.PaperIDs, paper.ID)
				if paper.Created.After(existing.LastCollab) {
					existing.LastCollab = paper.Created
				}
				if paper.Created.Before(existing.FirstCollab) {
					existing.FirstCollab = paper.Created
				}
			} else {
				collabMap[coauthorNorm] = &CollaboratorInfo{
					Author:      coauthorNorm,
					PaperCount:  1,
					PaperIDs:    []string{paper.ID},
					FirstCollab: paper.Created,
					LastCollab:  paper.Created,
				}
			}
		}
	}

	// Convert to slice and sort by paper count
	result := make([]CollaboratorInfo, 0, len(collabMap))
	for _, collab := range collabMap {
		result = append(result, *collab)
	}

	// Sort by paper count descending
	for i := 0; i < len(result)-1; i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].PaperCount > result[i].PaperCount {
				result[i], result[j] = result[j], result[i]
			}
		}
	}

	// Limit results
	if len(result) > limit {
		result = result[:limit]
	}

	return result, nil
}

// GetSimilarAuthors finds authors with similar research interests using embedding similarity.
func (c *Cache) GetSimilarAuthors(ctx context.Context, author string, limit int) ([]SimilarAuthor, error) {
	if c.dbType != DBTypePostgres {
		return nil, c.capabilityError("similar authors")
	}
	if limit <= 0 {
		limit = 10
	}

	// Get the author's embedding
	var authorEmbed AuthorEmbedding
	lookup := c.db.WithContext(ctx).Where("author = ?", author).Limit(1).Find(&authorEmbed)
	if lookup.Error != nil {
		return nil, fmt.Errorf("find author embedding: %w", lookup.Error)
	}
	if lookup.RowsAffected == 0 {
		return []SimilarAuthor{}, nil
	}

	// Find similar authors using cosine similarity
	var results []struct {
		Author     string
		PaperCount int
		Similarity float64
	}
	err := c.db.WithContext(ctx).Raw(`
		SELECT author, paper_count, 1 - (vector <=> (SELECT vector FROM author_embeddings WHERE author = ?)) AS similarity
		FROM author_embeddings
		WHERE author != ?
		ORDER BY vector <=> (SELECT vector FROM author_embeddings WHERE author = ?)
		LIMIT ?
	`, author, author, author, limit).Scan(&results).Error
	if err != nil {
		return nil, fmt.Errorf("find similar authors: %w", err)
	}

	similar := make([]SimilarAuthor, len(results))
	for i, r := range results {
		similar[i] = SimilarAuthor{
			Author:     r.Author,
			Similarity: r.Similarity,
			PaperCount: r.PaperCount,
		}
	}

	return similar, nil
}

// BuildAuthorGraph builds the collaboration graph from all papers.
// This is an expensive operation and should be run periodically.
func (c *Cache) BuildAuthorGraph(ctx context.Context) error {
	fmt.Println("Building author collaboration graph...")

	// Clear existing collaborations
	if err := c.db.WithContext(ctx).Exec("DELETE FROM author_collaborations").Error; err != nil {
		return fmt.Errorf("clear author collaborations: %w", err)
	}

	// Process papers in batches
	batchSize := 1000
	offset := 0
	collabMap := make(map[string]*AuthorCollaboration) // key: "author1|author2" sorted

	for {
		var papers []Paper
		err := c.db.WithContext(ctx).
			Select("id", "authors", "created").
			Order("id").
			Offset(offset).
			Limit(batchSize).
			Find(&papers).Error
		if err != nil {
			return fmt.Errorf("fetch papers: %w", err)
		}
		if len(papers) == 0 {
			break
		}

		for _, paper := range papers {
			authors := ParseAuthors(paper.Authors)
			if len(authors) < 2 {
				continue
			}

			// Create edges between all pairs of authors
			for i := 0; i < len(authors); i++ {
				for j := i + 1; j < len(authors); j++ {
					a1 := normalizeAuthor(authors[i])
					a2 := normalizeAuthor(authors[j])

					// Sort alphabetically for consistent key
					if a1 > a2 {
						a1, a2 = a2, a1
					}
					key := a1 + "|" + a2

					if existing, ok := collabMap[key]; ok {
						existing.PaperCount++
						var paperIDs []string
						if err := json.Unmarshal([]byte(existing.PaperIDs), &paperIDs); err != nil {
							return fmt.Errorf("decode collaboration paper IDs: %w", err)
						}
						paperIDs = append(paperIDs, paper.ID)
						paperIDsJSON, err := json.Marshal(paperIDs)
						if err != nil {
							return fmt.Errorf("encode collaboration paper IDs: %w", err)
						}
						existing.PaperIDs = string(paperIDsJSON)
						if paper.Created.Before(existing.FirstCollab) {
							existing.FirstCollab = paper.Created
						}
						if paper.Created.After(existing.LastCollab) {
							existing.LastCollab = paper.Created
						}
					} else {
						paperIDs, err := json.Marshal([]string{paper.ID})
						if err != nil {
							return fmt.Errorf("encode collaboration paper ID: %w", err)
						}
						collabMap[key] = &AuthorCollaboration{
							Author1:     a1,
							Author2:     a2,
							PaperCount:  1,
							PaperIDs:    string(paperIDs),
							FirstCollab: paper.Created,
							LastCollab:  paper.Created,
						}
					}
				}
			}
		}

		offset += batchSize
		if offset%10000 == 0 {
			fmt.Printf("Processed %d papers...\n", offset)
		}
	}

	// Batch insert collaborations
	fmt.Printf("Inserting %d collaboration edges...\n", len(collabMap))
	collabs := make([]AuthorCollaboration, 0, len(collabMap))
	for _, collab := range collabMap {
		collabs = append(collabs, *collab)
	}

	// Insert in batches
	insertBatch := 1000
	for i := 0; i < len(collabs); i += insertBatch {
		end := i + insertBatch
		if end > len(collabs) {
			end = len(collabs)
		}
		if err := c.db.WithContext(ctx).Create(collabs[i:end]).Error; err != nil {
			return fmt.Errorf("insert collaborations: %w", err)
		}
	}

	fmt.Println("Author collaboration graph built successfully")
	c.detailLRU.Clear()
	return nil
}

// BuildAuthorEmbeddings rebuilds the legacy MiniLM author index for migration workflows.
func (c *Cache) BuildAuthorEmbeddings(ctx context.Context) error {
	if c.dbType != DBTypePostgres {
		return c.capabilityError("author embeddings")
	}

	fmt.Println("Building author embeddings...")

	// Get unique authors and their papers with embeddings
	// This uses a single query to get author -> paper embeddings mapping
	err := c.db.WithContext(ctx).Exec(`
		INSERT INTO author_embeddings (author, paper_count, model, updated, vector)
		SELECT
			author,
			count(*) as paper_count,
			? as model,
			NOW() as updated,
			AVG(e.vector) as vector
		FROM (
			SELECT DISTINCT TRIM(unnest(string_to_array(replace(authors, ' and ', ', '), ','))) as author, id
			FROM papers
		) paper_authors
		JOIN embeddings e ON e.paper_id = paper_authors.id
		WHERE author != ''
		GROUP BY author
		HAVING count(*) >= 1
		ON CONFLICT (author) DO UPDATE SET
			paper_count = EXCLUDED.paper_count,
			updated = EXCLUDED.updated,
			vector = EXCLUDED.vector
	`, MiniLMEmbeddingModel).Error
	if err != nil {
		return fmt.Errorf("build author embeddings: %w", err)
	}

	var count int64
	if err := c.db.WithContext(ctx).Model(&AuthorEmbedding{}).Count(&count).Error; err != nil {
		return fmt.Errorf("count author embeddings: %w", err)
	}
	fmt.Printf("Built embeddings for %d authors\n", count)
	c.detailLRU.Clear()

	return nil
}

// HasAuthorEmbedding checks if an author has an embedding.
func (c *Cache) HasAuthorEmbedding(ctx context.Context, author string) bool {
	hasEmbedding, _ := c.HasAuthorEmbeddingResult(ctx, author)
	return hasEmbedding
}

// HasAuthorEmbeddingResult reports both availability and database failures.
func (c *Cache) HasAuthorEmbeddingResult(ctx context.Context, author string) (bool, error) {
	if c.dbType != DBTypePostgres {
		return false, nil
	}
	var count int64
	if err := c.db.WithContext(ctx).Model(&AuthorEmbedding{}).Where("author = ?", author).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// AuthorStats returns statistics about an author.
type AuthorStats struct {
	PaperCount        int  `json:"paper_count"`
	CollaboratorCount int  `json:"collaborator_count"`
	HasEmbedding      bool `json:"has_embedding"`
}

func (c *Cache) getAuthorPaperCountUncached(ctx context.Context, author string) (int64, error) {
	likeOp := "LIKE"
	if c.dbType == DBTypePostgres {
		likeOp = "ILIKE"
	}

	flipped := flipAuthorName(author)

	var count int64
	err := c.withAuthorQuery(ctx, func() error {
		if flipped != "" {
			return c.db.WithContext(ctx).Model(&Paper{}).Where("authors "+likeOp+" ? OR authors "+likeOp+" ?", "%"+author+"%", "%"+flipped+"%").Count(&count).Error
		}
		return c.db.WithContext(ctx).Model(&Paper{}).Where("authors "+likeOp+" ?", "%"+author+"%").Count(&count).Error
	})
	return count, err
}

// CountPapersByAuthor returns the number of cached papers by an author.
func (c *Cache) CountPapersByAuthor(ctx context.Context, author string) int64 {
	count, _ := c.CountPapersByAuthorResult(ctx, author)
	return count
}

// CountPapersByAuthorResult returns an exact count and preserves query errors.
func (c *Cache) CountPapersByAuthorResult(ctx context.Context, author string) (int64, error) {
	return c.getAuthorPaperCountUncached(ctx, author)
}

// GetAuthorStats returns statistics for an author.
func (c *Cache) GetAuthorStats(ctx context.Context, author string) (*AuthorStats, error) {
	cacheKey := detailKey("author_stats", author)
	if cached, ok := c.getDetailCache(cacheKey); ok {
		if stats, ok := cached.(*AuthorStats); ok {
			return cloneAuthorStats(stats), nil
		}
	}

	value, err, _ := c.detailFlights.Do(cacheKey, func() (interface{}, error) {
		if cached, ok := c.getDetailCache(cacheKey); ok {
			if stats, ok := cached.(*AuthorStats); ok {
				return cloneAuthorStats(stats), nil
			}
		}
		stats, err := c.getAuthorStatsUncached(ctx, author)
		if err != nil {
			return nil, err
		}
		c.putDetailCache(cacheKey, authorStatsTTL, cloneAuthorStats(stats))
		return stats, nil
	})
	if err != nil {
		return nil, err
	}
	stats, _ := value.(*AuthorStats)
	return cloneAuthorStats(stats), nil
}

func (c *Cache) getAuthorStatsUncached(ctx context.Context, author string) (*AuthorStats, error) {
	stats := &AuthorStats{}

	// Count papers using helper that searches both name formats
	paperCount, err := c.getAuthorPaperCountUncached(ctx, author)
	if err != nil {
		return nil, fmt.Errorf("count author papers: %w", err)
	}
	stats.PaperCount = int(paperCount)

	collaboratorCount, err := c.countAuthorCollaborators(ctx, author)
	if err != nil {
		return nil, fmt.Errorf("count author collaborators: %w", err)
	}
	stats.CollaboratorCount = collaboratorCount

	stats.HasEmbedding, err = c.HasAuthorEmbeddingResult(ctx, author)
	if err != nil {
		return nil, fmt.Errorf("check author embedding: %w", err)
	}

	return stats, nil
}

// CollabGraphNode represents a node in the collaboration graph.
type CollabGraphNode struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PaperCount int    `json:"paperCount"`
	IsCenter   bool   `json:"isCenter"`
}

// CollabGraphEdge represents an edge in the collaboration graph.
type CollabGraphEdge struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Weight int    `json:"weight"` // number of papers together
}

// AuthorGraph contains the collaboration network for visualization.
type AuthorGraph struct {
	Nodes []CollabGraphNode `json:"nodes"`
	Edges []CollabGraphEdge `json:"edges"`
}

// GetAuthorGraph returns the collaboration network for an author (2 levels deep).
func (c *Cache) GetAuthorGraph(ctx context.Context, author string, depth int) (*AuthorGraph, error) {
	if depth <= 0 {
		depth = 1
	}
	if depth > 2 {
		depth = 2
	}
	cacheKey := detailKey("author_graph", author, fmt.Sprint(depth))
	if cached, ok := c.getDetailCache(cacheKey); ok {
		if graph, ok := cached.(*AuthorGraph); ok {
			return cloneAuthorGraph(graph), nil
		}
	}

	value, err, _ := c.detailFlights.Do(cacheKey, func() (interface{}, error) {
		if cached, ok := c.getDetailCache(cacheKey); ok {
			if graph, ok := cached.(*AuthorGraph); ok {
				return cloneAuthorGraph(graph), nil
			}
		}
		graph, err := c.getAuthorGraphUncached(ctx, author, depth)
		if err != nil {
			return nil, err
		}
		c.putDetailCache(cacheKey, authorProfileTTL, cloneAuthorGraph(graph))
		return graph, nil
	})
	if err != nil {
		return nil, err
	}
	graph, _ := value.(*AuthorGraph)
	return cloneAuthorGraph(graph), nil
}

func (c *Cache) getAuthorGraphUncached(ctx context.Context, author string, depth int) (*AuthorGraph, error) {
	graph := &AuthorGraph{
		Nodes: []CollabGraphNode{},
		Edges: []CollabGraphEdge{},
	}

	// Track nodes we've added
	nodeSet := make(map[string]bool)
	edgeSet := make(map[string]bool)

	// Get center author's paper count using helper that searches both name formats
	centerPaperCount, err := c.getAuthorPaperCountUncached(ctx, author)
	if err != nil {
		return nil, fmt.Errorf("count center author papers: %w", err)
	}

	// Add center node
	graph.Nodes = append(graph.Nodes, CollabGraphNode{
		ID:         author,
		Name:       author,
		PaperCount: int(centerPaperCount),
		IsCenter:   true,
	})
	nodeSet[author] = true

	// Get first level collaborators
	collabs, err := c.GetCollaborators(ctx, author, 15)
	if err != nil {
		return nil, fmt.Errorf("get center collaborators: %w", err)
	}

	for _, collab := range collabs {
		collaboratorPaperCount, err := c.getAuthorPaperCountUncached(ctx, collab.Author)
		if err != nil {
			return nil, fmt.Errorf("count collaborator papers for %s: %w", collab.Author, err)
		}
		// Add collaborator node
		if !nodeSet[collab.Author] {
			graph.Nodes = append(graph.Nodes, CollabGraphNode{
				ID:         collab.Author,
				Name:       collab.Author,
				PaperCount: int(collaboratorPaperCount),
				IsCenter:   false,
			})
			nodeSet[collab.Author] = true
		}

		// Add edge
		edgeKey := author + "|" + collab.Author
		if !edgeSet[edgeKey] {
			graph.Edges = append(graph.Edges, CollabGraphEdge{
				Source: author,
				Target: collab.Author,
				Weight: collab.PaperCount,
			})
			edgeSet[edgeKey] = true
			edgeSet[collab.Author+"|"+author] = true
		}
	}

	// Get second level if depth > 1
	if depth > 1 {
		firstLevelAuthors := make([]string, len(collabs))
		for i, c := range collabs {
			firstLevelAuthors[i] = c.Author
		}

		for _, firstLevel := range firstLevelAuthors {
			secondCollabs, err := c.GetCollaborators(ctx, firstLevel, 5)
			if err != nil {
				return nil, fmt.Errorf("get second-level collaborators for %s: %w", firstLevel, err)
			}
			for _, collab := range secondCollabs {
				// Skip if it's the center author
				if collab.Author == author {
					continue
				}

				// Add node if new
				if !nodeSet[collab.Author] {
					paperCount, err := c.getAuthorPaperCountUncached(ctx, collab.Author)
					if err != nil {
						return nil, fmt.Errorf("count second-level author papers for %s: %w", collab.Author, err)
					}
					graph.Nodes = append(graph.Nodes, CollabGraphNode{
						ID:         collab.Author,
						Name:       collab.Author,
						PaperCount: int(paperCount),
						IsCenter:   false,
					})
					nodeSet[collab.Author] = true
				}

				// Add edge
				edgeKey := firstLevel + "|" + collab.Author
				if !edgeSet[edgeKey] {
					graph.Edges = append(graph.Edges, CollabGraphEdge{
						Source: firstLevel,
						Target: collab.Author,
						Weight: collab.PaperCount,
					})
					edgeSet[edgeKey] = true
					edgeSet[collab.Author+"|"+firstLevel] = true
				}
			}
		}
	}

	return graph, nil
}

// ResearchArea represents a research area with paper count.
type ResearchArea struct {
	Category   string  `json:"category"`
	PaperCount int     `json:"paper_count"`
	Percentage float64 `json:"percentage"`
}

// YearlyOutput represents papers published in a year.
type YearlyOutput struct {
	Year       int `json:"year"`
	PaperCount int `json:"paper_count"`
}

// AuthorProfile contains comprehensive profile information.
type AuthorProfile struct {
	Name               string         `json:"name"`
	TotalPapers        int            `json:"total_papers"`
	TotalCollaborators int            `json:"total_collaborators"`
	ResearchAreas      []ResearchArea `json:"research_areas"`
	YearlyOutput       []YearlyOutput `json:"yearly_output"`
	FirstPaper         *time.Time     `json:"first_paper"`
	LastPaper          *time.Time     `json:"last_paper"`
	HasEmbedding       bool           `json:"has_embedding"`
}

// GetAuthorProfile returns comprehensive profile for an author.
func (c *Cache) GetAuthorProfile(ctx context.Context, author string) (*AuthorProfile, error) {
	cacheKey := detailKey("author_profile", author)
	if cached, ok := c.getDetailCache(cacheKey); ok {
		if profile, ok := cached.(*AuthorProfile); ok {
			return cloneAuthorProfile(profile), nil
		}
	}

	value, err, _ := c.detailFlights.Do(cacheKey, func() (interface{}, error) {
		if cached, ok := c.getDetailCache(cacheKey); ok {
			if profile, ok := cached.(*AuthorProfile); ok {
				return cloneAuthorProfile(profile), nil
			}
		}
		profile, err := c.getAuthorProfileUncached(ctx, author)
		if err != nil {
			return nil, err
		}
		c.putDetailCache(cacheKey, authorProfileTTL, cloneAuthorProfile(profile))
		return profile, nil
	})
	if err != nil {
		return nil, err
	}
	profile, _ := value.(*AuthorProfile)
	return cloneAuthorProfile(profile), nil
}

func (c *Cache) getAuthorProfileUncached(ctx context.Context, author string) (*AuthorProfile, error) {
	profile := &AuthorProfile{
		Name:          author,
		ResearchAreas: []ResearchArea{},
		YearlyOutput:  []YearlyOutput{},
	}

	likeOp := "LIKE"
	if c.dbType == DBTypePostgres {
		likeOp = "ILIKE"
	}

	// Search for both "First Last" and "Last, First" formats
	flipped := flipAuthorName(author)

	// Get all papers by author
	var papers []Paper
	err := c.withAuthorQuery(ctx, func() error {
		if flipped != "" {
			return c.db.WithContext(ctx).
				Select("id", "categories", "created").
				Where("authors "+likeOp+" ? OR authors "+likeOp+" ?", "%"+author+"%", "%"+flipped+"%").
				Order("created ASC").
				Find(&papers).Error
		}
		return c.db.WithContext(ctx).
			Select("id", "categories", "created").
			Where("authors "+likeOp+" ?", "%"+author+"%").
			Order("created ASC").
			Find(&papers).Error
	})
	if err != nil {
		return nil, err
	}

	profile.TotalPapers = len(papers)

	if len(papers) > 0 {
		profile.FirstPaper = &papers[0].Created
		profile.LastPaper = &papers[len(papers)-1].Created
	}

	// Count research areas (categories)
	categoryCount := make(map[string]int)
	yearCount := make(map[int]int)

	for _, p := range papers {
		// Parse categories
		for _, cat := range strings.Split(p.Categories, " ") {
			cat = strings.TrimSpace(cat)
			if cat != "" {
				categoryCount[cat]++
			}
		}

		// Count by year
		year := p.Created.Year()
		if year > 1990 && year < 2100 { // sanity check
			yearCount[year]++
		}
	}

	// Convert to sorted slices
	type catCount struct {
		cat   string
		count int
	}
	var cats []catCount
	for cat, count := range categoryCount {
		cats = append(cats, catCount{cat, count})
	}
	// Sort by count descending
	for i := 0; i < len(cats); i++ {
		for j := i + 1; j < len(cats); j++ {
			if cats[j].count > cats[i].count {
				cats[i], cats[j] = cats[j], cats[i]
			}
		}
	}
	// Take top 10
	for i, cc := range cats {
		if i >= 10 {
			break
		}
		profile.ResearchAreas = append(profile.ResearchAreas, ResearchArea{
			Category:   cc.cat,
			PaperCount: cc.count,
			Percentage: float64(cc.count) / float64(len(papers)) * 100,
		})
	}

	// Convert yearly output
	var years []int
	for year := range yearCount {
		years = append(years, year)
	}
	// Sort years
	for i := 0; i < len(years); i++ {
		for j := i + 1; j < len(years); j++ {
			if years[j] < years[i] {
				years[i], years[j] = years[j], years[i]
			}
		}
	}
	for _, year := range years {
		profile.YearlyOutput = append(profile.YearlyOutput, YearlyOutput{
			Year:       year,
			PaperCount: yearCount[year],
		})
	}

	profile.TotalCollaborators, err = c.countAuthorCollaborators(ctx, author)
	if err != nil {
		return nil, fmt.Errorf("count author collaborators: %w", err)
	}

	profile.HasEmbedding, err = c.HasAuthorEmbeddingResult(ctx, author)
	if err != nil {
		return nil, fmt.Errorf("check author embedding: %w", err)
	}

	return profile, nil
}

func (c *Cache) countAuthorCollaborators(ctx context.Context, author string) (int, error) {
	collaborators, err := c.getCollaboratorsUncached(ctx, author, int(^uint(0)>>1))
	if err != nil {
		return 0, err
	}
	return len(collaborators), nil
}
