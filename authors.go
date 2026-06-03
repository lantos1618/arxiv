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
	// Replace " and " with comma for consistent splitting
	authors = strings.ReplaceAll(authors, " and ", ", ")

	var result []string
	for _, a := range strings.Split(authors, ",") {
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
	if flipped != "" {
		err = c.db.WithContext(ctx).
			Where("authors "+likeOp+" ? OR authors "+likeOp+" ?", "%"+author+"%", "%"+flipped+"%").
			Select("id", "authors", "created").
			Find(&papers).Error
	} else {
		err = c.db.WithContext(ctx).
			Where("authors "+likeOp+" ?", "%"+author+"%").
			Select("id", "authors", "created").
			Find(&papers).Error
	}
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
		return nil, fmt.Errorf("similar authors requires PostgreSQL with pgvector")
	}
	if limit <= 0 {
		limit = 10
	}

	// Get the author's embedding
	var authorEmbed AuthorEmbedding
	err := c.db.WithContext(ctx).Where("author = ?", author).First(&authorEmbed).Error
	if err != nil {
		return nil, fmt.Errorf("author embedding not found: %w", err)
	}

	// Find similar authors using cosine similarity
	var results []struct {
		Author     string
		PaperCount int
		Similarity float64
	}
	err = c.db.WithContext(ctx).Raw(`
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
	c.db.WithContext(ctx).Exec("DELETE FROM author_collaborations")

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
						json.Unmarshal([]byte(existing.PaperIDs), &paperIDs)
						paperIDs = append(paperIDs, paper.ID)
						paperIDsJSON, _ := json.Marshal(paperIDs)
						existing.PaperIDs = string(paperIDsJSON)
						if paper.Created.Before(existing.FirstCollab) {
							existing.FirstCollab = paper.Created
						}
						if paper.Created.After(existing.LastCollab) {
							existing.LastCollab = paper.Created
						}
					} else {
						paperIDs, _ := json.Marshal([]string{paper.ID})
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
	return nil
}

// BuildAuthorEmbeddings computes embeddings for all authors by averaging their paper embeddings.
func (c *Cache) BuildAuthorEmbeddings(ctx context.Context) error {
	if c.dbType != DBTypePostgres {
		return fmt.Errorf("author embeddings require PostgreSQL with pgvector")
	}

	fmt.Println("Building author embeddings...")

	// Get unique authors and their papers with embeddings
	// This uses a single query to get author -> paper embeddings mapping
	err := c.db.WithContext(ctx).Exec(`
		INSERT INTO author_embeddings (author, paper_count, model, updated, vector)
		SELECT
			author,
			count(*) as paper_count,
			'all-MiniLM-L6-v2' as model,
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
	`).Error
	if err != nil {
		return fmt.Errorf("build author embeddings: %w", err)
	}

	var count int64
	c.db.WithContext(ctx).Model(&AuthorEmbedding{}).Count(&count)
	fmt.Printf("Built embeddings for %d authors\n", count)

	return nil
}

// HasAuthorEmbedding checks if an author has an embedding.
func (c *Cache) HasAuthorEmbedding(ctx context.Context, author string) bool {
	var count int64
	c.db.WithContext(ctx).Model(&AuthorEmbedding{}).Where("author = ?", author).Count(&count)
	return count > 0
}

// AuthorStats returns statistics about an author.
type AuthorStats struct {
	PaperCount        int  `json:"paper_count"`
	CollaboratorCount int  `json:"collaborator_count"`
	HasEmbedding      bool `json:"has_embedding"`
}

// getAuthorPaperCount returns the number of papers by an author, searching both name formats.
func (c *Cache) getAuthorPaperCount(ctx context.Context, author string) int64 {
	likeOp := "LIKE"
	if c.dbType == DBTypePostgres {
		likeOp = "ILIKE"
	}

	flipped := flipAuthorName(author)

	var count int64
	if flipped != "" {
		c.db.WithContext(ctx).Model(&Paper{}).Where("authors "+likeOp+" ? OR authors "+likeOp+" ?", "%"+author+"%", "%"+flipped+"%").Count(&count)
	} else {
		c.db.WithContext(ctx).Model(&Paper{}).Where("authors "+likeOp+" ?", "%"+author+"%").Count(&count)
	}
	return count
}

// CountPapersByAuthor returns the number of cached papers by an author.
func (c *Cache) CountPapersByAuthor(ctx context.Context, author string) int64 {
	return c.getAuthorPaperCount(ctx, author)
}

// GetAuthorStats returns statistics for an author.
func (c *Cache) GetAuthorStats(ctx context.Context, author string) (*AuthorStats, error) {
	stats := &AuthorStats{}

	// Count papers using helper that searches both name formats
	stats.PaperCount = int(c.getAuthorPaperCount(ctx, author))

	// Count unique collaborators
	var collabCount int64
	c.db.WithContext(ctx).Model(&AuthorCollaboration{}).Where("author1 = ? OR author2 = ?", author, author).Count(&collabCount)
	stats.CollaboratorCount = int(collabCount)

	// Check embedding
	stats.HasEmbedding = c.HasAuthorEmbedding(ctx, author)

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

	graph := &AuthorGraph{
		Nodes: []CollabGraphNode{},
		Edges: []CollabGraphEdge{},
	}

	// Track nodes we've added
	nodeSet := make(map[string]bool)
	edgeSet := make(map[string]bool)

	// Get center author's paper count using helper that searches both name formats
	centerPaperCount := c.getAuthorPaperCount(ctx, author)

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
		return graph, nil // Return partial graph
	}

	for _, collab := range collabs {
		// Add collaborator node
		if !nodeSet[collab.Author] {
			pc := c.getAuthorPaperCount(ctx, collab.Author)
			graph.Nodes = append(graph.Nodes, CollabGraphNode{
				ID:         collab.Author,
				Name:       collab.Author,
				PaperCount: int(pc),
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
			secondCollabs, _ := c.GetCollaborators(ctx, firstLevel, 5)
			for _, collab := range secondCollabs {
				// Skip if it's the center author
				if collab.Author == author {
					continue
				}

				// Add node if new
				if !nodeSet[collab.Author] {
					pc := c.getAuthorPaperCount(ctx, collab.Author)
					graph.Nodes = append(graph.Nodes, CollabGraphNode{
						ID:         collab.Author,
						Name:       collab.Author,
						PaperCount: int(pc),
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
	if flipped != "" {
		c.db.WithContext(ctx).
			Select("id", "categories", "created").
			Where("authors "+likeOp+" ? OR authors "+likeOp+" ?", "%"+author+"%", "%"+flipped+"%").
			Order("created ASC").
			Find(&papers)
	} else {
		c.db.WithContext(ctx).
			Select("id", "categories", "created").
			Where("authors "+likeOp+" ?", "%"+author+"%").
			Order("created ASC").
			Find(&papers)
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

	// Get collaborator count
	var collabCount int64
	c.db.WithContext(ctx).Model(&AuthorCollaboration{}).Where("author1 = ? OR author2 = ?", author, author).Count(&collabCount)
	profile.TotalCollaborators = int(collabCount)

	// Check embedding
	profile.HasEmbedding = c.HasAuthorEmbedding(ctx, author)

	return profile, nil
}
