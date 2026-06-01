package arxiv

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

type SemanticResult struct {
	PaperID    string  `json:"paperId"`
	Similarity float64 `json:"similarity"`
	Paper      *Paper  `json:"paper,omitempty"`
}

type DeepSearchResult struct {
	PaperID    string  `json:"paperId"`
	Similarity float64 `json:"similarity"`
	ChunkID    string  `json:"chunkId"`
	Section    string  `json:"section,omitempty"`
	Snippet    string  `json:"snippet,omitempty"`
	Paper      *Paper  `json:"paper,omitempty"`
}

type SemanticMap struct {
	Points     []SemanticMapPoint `json:"points"`
	Links      []SemanticMapLink  `json:"links"`
	Clusters   []SemanticCluster  `json:"clusters"`
	Projection string             `json:"projection"`
	Dimensions int                `json:"dimensions"`
}

type SemanticMapPoint struct {
	PaperID      string  `json:"paperId"`
	Title        string  `json:"title,omitempty"`
	Categories   string  `json:"categories,omitempty"`
	Similarity   float64 `json:"similarity"`
	X            float64 `json:"x"`
	Y            float64 `json:"y"`
	Cluster      int     `json:"cluster"`
	ClusterLabel string  `json:"clusterLabel"`
	Anchor       bool    `json:"anchor"`
}

type SemanticMapLink struct {
	Source   string  `json:"source"`
	Target   string  `json:"target"`
	Strength float64 `json:"strength"`
}

type SemanticCluster struct {
	ID    int     `json:"id"`
	Label string  `json:"label"`
	Count int     `json:"count"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

type semanticVectorRow struct {
	PaperID    string
	Similarity float64
	Vector     []float64
	Anchor     bool
}

type semanticTermScore struct {
	term  string
	score float64
}

// SearchSemantic performs semantic search using pgvector's native similarity search.
// Uses cosine distance operator (<=>) for fast approximate nearest neighbor search.
func (c *Cache) SearchSemantic(ctx context.Context, queryEmbedding []float32, limit int) ([]SemanticResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if c.dbType != DBTypePostgres {
		return nil, fmt.Errorf("semantic search requires PostgreSQL with pgvector")
	}

	// Convert query embedding to pgvector format: [0.1,0.2,...]
	vecStr := float32SliceToVectorString(queryEmbedding)

	// Use pgvector's cosine distance operator (<=>)
	// 1 - distance = similarity
	query := `
		SELECT paper_id, 1 - (vector <=> $1::vector) as similarity
		FROM embeddings
		WHERE vector IS NOT NULL
		ORDER BY vector <=> $1::vector
		LIMIT $2
	`

	sqlDB, err := c.db.DB()
	if err != nil {
		return nil, err
	}

	rows, err := sqlDB.QueryContext(ctx, query, vecStr, limit)
	if err != nil {
		return nil, fmt.Errorf("semantic search query failed: %w", err)
	}
	defer rows.Close()

	var results []SemanticResult
	for rows.Next() {
		var r SemanticResult
		if err := rows.Scan(&r.PaperID, &r.Similarity); err != nil {
			return nil, err
		}
		results = append(results, r)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return c.attachPaperDetails(ctx, results), nil
}

// SearchSemanticQwen performs abstract-level semantic search over Qwen vectors.
func (c *Cache) SearchSemanticQwen(ctx context.Context, queryEmbedding []float32, limit int) ([]SemanticResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if c.dbType != DBTypePostgres {
		return nil, fmt.Errorf("qwen semantic search requires PostgreSQL with pgvector")
	}

	vecStr := float32SliceToVectorString(queryEmbedding)
	query := `
		SELECT paper_id, 1 - (vector <=> $1::vector) as similarity
		FROM embeddings_v2
		WHERE scope = 'abstract'
		  AND model = $2
		  AND dim = $3
		  AND vector IS NOT NULL
		ORDER BY vector <=> $1::vector
		LIMIT $4
	`

	sqlDB, err := c.db.DB()
	if err != nil {
		return nil, err
	}

	rows, err := sqlDB.QueryContext(ctx, query, vecStr, qwenEmbeddingModel, qwenEmbeddingDim, limit)
	if err != nil {
		return nil, fmt.Errorf("qwen semantic search query failed: %w", err)
	}
	defer rows.Close()

	var results []SemanticResult
	for rows.Next() {
		var r SemanticResult
		if err := rows.Scan(&r.PaperID, &r.Similarity); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return c.attachPaperDetails(ctx, results), nil
}

// SearchDeepQwen searches full-paper chunks and returns one best chunk per paper.
func (c *Cache) SearchDeepQwen(ctx context.Context, queryEmbedding []float32, limit int) ([]DeepSearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	if c.dbType != DBTypePostgres {
		return nil, fmt.Errorf("deep search requires PostgreSQL with pgvector")
	}

	vecStr := float32SliceToVectorString(queryEmbedding)
	candidateLimit := limit * 8
	if candidateLimit < 80 {
		candidateLimit = 80
	}
	query := `
		WITH nearest AS (
			SELECT c.paper_id,
			       c.id AS chunk_id,
			       c.section,
			       left(regexp_replace(c.text, '[[:space:]]+', ' ', 'g'), 520) AS snippet,
			       e.vector <=> $1::vector AS distance,
			       1 - (e.vector <=> $1::vector) AS similarity
			FROM chunk_embeddings_v2 e
			JOIN paper_chunks c ON c.id = e.chunk_id
			WHERE c.scope = 'pdf_text'
			  AND e.model = $2
			  AND e.dim = $3
			  AND e.vector IS NOT NULL
			  AND COALESCE(c.text, '') <> ''
			ORDER BY e.vector <=> $1::vector
			LIMIT $4
		),
		ranked AS (
			SELECT *,
			       row_number() OVER (PARTITION BY paper_id ORDER BY distance) AS rn
			FROM nearest
		)
		SELECT paper_id, similarity, chunk_id, COALESCE(section, ''), COALESCE(snippet, '')
		FROM ranked
		WHERE rn = 1
		ORDER BY distance
		LIMIT $5
	`

	sqlDB, err := c.db.DB()
	if err != nil {
		return nil, err
	}

	rows, err := sqlDB.QueryContext(ctx, query, vecStr, qwenEmbeddingModel, qwenEmbeddingDim, candidateLimit, limit)
	if err != nil {
		return nil, fmt.Errorf("deep search query failed: %w", err)
	}
	defer rows.Close()

	results := []DeepSearchResult{}
	ids := []string{}
	for rows.Next() {
		var r DeepSearchResult
		if err := rows.Scan(&r.PaperID, &r.Similarity, &r.ChunkID, &r.Section, &r.Snippet); err != nil {
			return nil, err
		}
		results = append(results, r)
		ids = append(ids, r.PaperID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if papers, err := c.GetPapersByIDs(ctx, ids); err == nil {
		paperMap := make(map[string]*Paper, len(papers))
		for _, paper := range papers {
			p := paper
			paperMap[paper.ID] = &p
		}
		for i := range results {
			results[i].Paper = paperMap[results[i].PaperID]
		}
	}

	return results, nil
}

// SimilarPaperMap returns a true embedding-space map around a paper.
func (c *Cache) SimilarPaperMap(ctx context.Context, paperID string, limit int) (*SemanticMap, []SemanticResult, error) {
	if limit <= 0 {
		limit = 80
	}
	if c.dbType != DBTypePostgres {
		return nil, nil, fmt.Errorf("similar paper maps require PostgreSQL with pgvector")
	}

	query := `
		WITH anchor AS (
			SELECT paper_id, vector
			FROM embeddings
			WHERE paper_id = $1
			  AND vector IS NOT NULL
			LIMIT 1
		),
		neighbors AS (
			SELECT e.paper_id,
			       1 - (e.vector <=> a.vector) AS similarity,
			       e.vector::text AS vector_text,
			       false AS is_anchor
			FROM embeddings e
			CROSS JOIN anchor a
			WHERE e.paper_id <> $1
			  AND e.vector IS NOT NULL
			ORDER BY e.vector <=> a.vector
			LIMIT $2
		)
		SELECT paper_id, similarity, vector_text, is_anchor
		FROM (
			SELECT paper_id, 1.0::double precision AS similarity, vector::text AS vector_text, true AS is_anchor
			FROM anchor
			UNION ALL
			SELECT paper_id, similarity, vector_text, is_anchor
			FROM neighbors
		) mapped
	`

	sqlDB, err := c.db.DB()
	if err != nil {
		return nil, nil, err
	}

	rows, err := sqlDB.QueryContext(ctx, query, paperID, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("similar paper map query failed: %w", err)
	}
	defer rows.Close()

	vectorRows := []semanticVectorRow{}
	for rows.Next() {
		var r semanticVectorRow
		var vectorText string
		if err := rows.Scan(&r.PaperID, &r.Similarity, &vectorText, &r.Anchor); err != nil {
			return nil, nil, err
		}
		r.Vector, err = parsePgVectorText(vectorText)
		if err != nil {
			return nil, nil, fmt.Errorf("parse vector for %s: %w", r.PaperID, err)
		}
		vectorRows = append(vectorRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(vectorRows) == 0 {
		return nil, nil, fmt.Errorf("paper embedding not found")
	}

	sort.SliceStable(vectorRows, func(i, j int) bool {
		if vectorRows[i].Anchor != vectorRows[j].Anchor {
			return vectorRows[i].Anchor
		}
		return vectorRows[i].Similarity > vectorRows[j].Similarity
	})

	results := make([]SemanticResult, 0, len(vectorRows)-1)
	ids := make([]string, 0, len(vectorRows))
	for _, row := range vectorRows {
		ids = append(ids, row.PaperID)
		if !row.Anchor {
			results = append(results, SemanticResult{
				PaperID:    row.PaperID,
				Similarity: row.Similarity,
			})
		}
	}

	paperMap := map[string]*Paper{}
	if papers, err := c.GetPapersByIDs(ctx, ids); err == nil {
		for _, paper := range papers {
			p := paper
			paperMap[paper.ID] = &p
		}
	}

	for i := range results {
		if paper, ok := paperMap[results[i].PaperID]; ok {
			results[i].Paper = paper
		}
	}

	return buildSemanticMap(vectorRows, paperMap), results, nil
}

// SimilarPaperMapQwen returns a related-work map from Qwen abstract embeddings.
func (c *Cache) SimilarPaperMapQwen(ctx context.Context, paperID string, limit int) (*SemanticMap, []SemanticResult, error) {
	if limit <= 0 {
		limit = 80
	}
	if c.dbType != DBTypePostgres {
		return nil, nil, fmt.Errorf("similar paper maps require PostgreSQL with pgvector")
	}

	sqlDB, err := c.db.DB()
	if err != nil {
		return nil, nil, err
	}

	var anchorVectorText string
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT vector::text
		FROM embeddings_v2
		WHERE paper_id = $1
		  AND scope = 'abstract'
		  AND model = $2
		  AND dim = $3
		  AND vector IS NOT NULL
		LIMIT 1
	`, paperID, qwenEmbeddingModel, qwenEmbeddingDim).Scan(&anchorVectorText); err != nil {
		return nil, nil, fmt.Errorf("paper qwen embedding not found: %w", err)
	}

	anchorVector, err := parsePgVectorText(anchorVectorText)
	if err != nil {
		return nil, nil, fmt.Errorf("parse vector for %s: %w", paperID, err)
	}

	query := `
		SELECT e.paper_id,
		       1 - (e.vector <=> $1::vector) AS similarity,
		       e.vector::text AS vector_text
		FROM embeddings_v2 e
		WHERE e.paper_id <> $2
		  AND e.scope = 'abstract'
		  AND e.model = $3
		  AND e.dim = $4
		  AND e.vector IS NOT NULL
		ORDER BY e.vector <=> $1::vector
		LIMIT $5
	`

	rows, err := sqlDB.QueryContext(ctx, query, anchorVectorText, paperID, qwenEmbeddingModel, qwenEmbeddingDim, limit)
	if err != nil {
		return nil, nil, fmt.Errorf("qwen similar paper map query failed: %w", err)
	}
	defer rows.Close()

	vectorRows := []semanticVectorRow{{
		PaperID:    paperID,
		Similarity: 1,
		Vector:     anchorVector,
		Anchor:     true,
	}}
	for rows.Next() {
		var r semanticVectorRow
		var vectorText string
		if err := rows.Scan(&r.PaperID, &r.Similarity, &vectorText); err != nil {
			return nil, nil, err
		}
		r.Vector, err = parsePgVectorText(vectorText)
		if err != nil {
			return nil, nil, fmt.Errorf("parse vector for %s: %w", r.PaperID, err)
		}
		vectorRows = append(vectorRows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(vectorRows) == 0 {
		return nil, nil, fmt.Errorf("paper qwen embedding not found")
	}

	sort.SliceStable(vectorRows, func(i, j int) bool {
		if vectorRows[i].Anchor != vectorRows[j].Anchor {
			return vectorRows[i].Anchor
		}
		return vectorRows[i].Similarity > vectorRows[j].Similarity
	})

	results := make([]SemanticResult, 0, len(vectorRows)-1)
	ids := make([]string, 0, len(vectorRows))
	for _, row := range vectorRows {
		ids = append(ids, row.PaperID)
		if !row.Anchor {
			results = append(results, SemanticResult{
				PaperID:    row.PaperID,
				Similarity: row.Similarity,
			})
		}
	}

	paperMap := map[string]*Paper{}
	if papers, err := c.GetPapersByIDs(ctx, ids); err == nil {
		for _, paper := range papers {
			p := paper
			paperMap[paper.ID] = &p
		}
	}

	for i := range results {
		if paper, ok := paperMap[results[i].PaperID]; ok {
			results[i].Paper = paper
		}
	}

	return buildSemanticMap(vectorRows, paperMap), results, nil
}

// SimilarPapers finds papers nearest to an existing paper embedding.
func (c *Cache) SimilarPapers(ctx context.Context, paperID string, limit int) ([]SemanticResult, error) {
	if limit <= 0 {
		limit = 24
	}
	if c.dbType != DBTypePostgres {
		return nil, fmt.Errorf("similar papers require PostgreSQL with pgvector")
	}

	query := `
		SELECT paper_id,
		       1 - (vector <=> (SELECT vector FROM embeddings WHERE paper_id = $1 AND vector IS NOT NULL)) AS similarity
		FROM embeddings
		WHERE paper_id <> $1
		  AND vector IS NOT NULL
		ORDER BY vector <=> (SELECT vector FROM embeddings WHERE paper_id = $1 AND vector IS NOT NULL)
		LIMIT $2
	`

	sqlDB, err := c.db.DB()
	if err != nil {
		return nil, err
	}

	rows, err := sqlDB.QueryContext(ctx, query, paperID, limit)
	if err != nil {
		return nil, fmt.Errorf("similar papers query failed: %w", err)
	}
	defer rows.Close()

	var results []SemanticResult
	for rows.Next() {
		var r SemanticResult
		if err := rows.Scan(&r.PaperID, &r.Similarity); err != nil {
			return nil, err
		}
		results = append(results, r)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return c.attachPaperDetails(ctx, results), nil
}

// float32SliceToVectorString converts a float32 slice to pgvector format: [0.1,0.2,...]
func float32SliceToVectorString(v []float32) string {
	strs := make([]string, len(v))
	for i, f := range v {
		strs[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(strs, ",") + "]"
}

func parsePgVectorText(text string) ([]float64, error) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "[")
	text = strings.TrimSuffix(text, "]")
	if text == "" {
		return nil, fmt.Errorf("empty vector")
	}
	parts := strings.Split(text, ",")
	vector := make([]float64, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return nil, err
		}
		vector = append(vector, value)
	}
	return vector, nil
}

func buildSemanticMap(rows []semanticVectorRow, paperMap map[string]*Paper) *SemanticMap {
	coords, dimensions := projectEmbeddingPCA(rows)
	assignments := clusterSemanticCoordinates(coords, rows)
	clusters, labels := labelSemanticClusters(rows, coords, assignments, paperMap)
	links := buildSemanticLinks(rows)

	points := make([]SemanticMapPoint, len(rows))
	for i, row := range rows {
		cluster := 0
		if i < len(assignments) {
			cluster = assignments[i]
		}
		points[i] = SemanticMapPoint{
			PaperID:      row.PaperID,
			Similarity:   row.Similarity,
			X:            roundCoord(coords[i][0]),
			Y:            roundCoord(coords[i][1]),
			Cluster:      cluster,
			ClusterLabel: labels[cluster],
			Anchor:       row.Anchor,
		}
		if paper := paperMap[row.PaperID]; paper != nil {
			points[i].Title = paper.Title
			points[i].Categories = paper.Categories
		}
	}

	return &SemanticMap{
		Points:     points,
		Links:      links,
		Clusters:   clusters,
		Projection: "pca-2d",
		Dimensions: dimensions,
	}
}

func projectEmbeddingPCA(rows []semanticVectorRow) ([][2]float64, int) {
	coords := make([][2]float64, len(rows))
	if len(rows) == 0 {
		return coords, 0
	}

	dim := len(rows[0].Vector)
	for _, row := range rows {
		if len(row.Vector) < dim {
			dim = len(row.Vector)
		}
	}
	if dim == 0 || len(rows) == 1 {
		return coords, dim
	}

	mean := make([]float64, dim)
	for _, row := range rows {
		for j := 0; j < dim; j++ {
			mean[j] += row.Vector[j]
		}
	}
	for j := range mean {
		mean[j] /= float64(len(rows))
	}

	centered := make([][]float64, len(rows))
	for i, row := range rows {
		centered[i] = make([]float64, dim)
		for j := 0; j < dim; j++ {
			centered[i][j] = row.Vector[j] - mean[j]
		}
	}

	pc1 := principalComponent(centered, nil)
	xScores := projectScores(centered, pc1)
	deflated := deflateByComponent(centered, pc1, xScores)
	pc2 := principalComponent(deflated, pc1)
	yScores := projectScores(centered, pc2)

	anchorX := xScores[0]
	anchorY := yScores[0]
	maxRadius := 0.0
	for i := range rows {
		coords[i][0] = xScores[i] - anchorX
		coords[i][1] = yScores[i] - anchorY
		maxRadius = math.Max(maxRadius, math.Hypot(coords[i][0], coords[i][1]))
	}

	if maxRadius < 1e-9 {
		for i := 1; i < len(coords); i++ {
			angle := 2 * math.Pi * float64(i-1) / math.Max(1, float64(len(coords)-1))
			radius := 0.35 + 0.55*float64(i)/float64(len(coords))
			coords[i][0] = math.Cos(angle) * radius
			coords[i][1] = math.Sin(angle) * radius
		}
		return coords, dim
	}

	for i := range coords {
		coords[i][0] /= maxRadius
		coords[i][1] /= maxRadius
	}

	return coords, dim
}

func principalComponent(data [][]float64, orthogonal []float64) []float64 {
	if len(data) == 0 || len(data[0]) == 0 {
		return nil
	}
	dim := len(data[0])
	component := make([]float64, dim)
	for i := range component {
		component[i] = math.Sin(float64(i+1)*12.9898) + 0.5*math.Cos(float64(i+1)*78.233)
	}
	orthogonalize(component, orthogonal)
	if normalize(component) < 1e-12 {
		component[0] = 1
	}

	for iter := 0; iter < 28; iter++ {
		next := make([]float64, dim)
		for _, row := range data {
			score := dot(row, component)
			for j := 0; j < dim; j++ {
				next[j] += row[j] * score
			}
		}
		orthogonalize(next, orthogonal)
		if normalize(next) < 1e-12 {
			return component
		}
		component = next
	}
	return component
}

func projectScores(data [][]float64, component []float64) []float64 {
	scores := make([]float64, len(data))
	if len(component) == 0 {
		return scores
	}
	for i, row := range data {
		scores[i] = dot(row, component)
	}
	return scores
}

func deflateByComponent(data [][]float64, component []float64, scores []float64) [][]float64 {
	deflated := make([][]float64, len(data))
	for i, row := range data {
		deflated[i] = make([]float64, len(row))
		copy(deflated[i], row)
		if len(component) == 0 {
			continue
		}
		for j := range row {
			deflated[i][j] -= scores[i] * component[j]
		}
	}
	return deflated
}

func clusterSemanticCoordinates(coords [][2]float64, rows []semanticVectorRow) []int {
	assignments := make([]int, len(coords))
	if len(coords) <= 1 {
		return assignments
	}

	clusterable := len(coords) - 1
	k := int(math.Round(math.Sqrt(float64(clusterable)) / 2))
	if clusterable >= 16 && k < 3 {
		k = 3
	}
	if k < 1 {
		k = 1
	}
	if k > 7 {
		k = 7
	}
	if k > clusterable {
		k = clusterable
	}
	if k <= 1 {
		return assignments
	}

	centroids := seedCentroids(coords, k)
	for iter := 0; iter < 24; iter++ {
		changed := false
		for i := range coords {
			next := nearestCentroid(coords[i], centroids)
			if assignments[i] != next {
				assignments[i] = next
				changed = true
			}
		}

		sums := make([][2]float64, k)
		counts := make([]int, k)
		for i, coord := range coords {
			cluster := assignments[i]
			if rows[i].Anchor && len(coords) > 2 {
				continue
			}
			sums[cluster][0] += coord[0]
			sums[cluster][1] += coord[1]
			counts[cluster]++
		}
		for cluster := 0; cluster < k; cluster++ {
			if counts[cluster] == 0 {
				continue
			}
			centroids[cluster][0] = sums[cluster][0] / float64(counts[cluster])
			centroids[cluster][1] = sums[cluster][1] / float64(counts[cluster])
		}
		if !changed {
			break
		}
	}

	return assignments
}

func seedCentroids(coords [][2]float64, k int) [][2]float64 {
	centroids := make([][2]float64, 0, k)
	first := 1
	if first >= len(coords) {
		first = 0
	}
	centroids = append(centroids, coords[first])
	for len(centroids) < k {
		bestIndex := first
		bestDistance := -1.0
		for i := 1; i < len(coords); i++ {
			minDistance := math.MaxFloat64
			for _, centroid := range centroids {
				minDistance = math.Min(minDistance, squaredDistance(coords[i], centroid))
			}
			if minDistance > bestDistance {
				bestDistance = minDistance
				bestIndex = i
			}
		}
		centroids = append(centroids, coords[bestIndex])
	}
	return centroids
}

func nearestCentroid(coord [2]float64, centroids [][2]float64) int {
	best := 0
	bestDistance := math.MaxFloat64
	for i, centroid := range centroids {
		distance := squaredDistance(coord, centroid)
		if distance < bestDistance {
			bestDistance = distance
			best = i
		}
	}
	return best
}

func labelSemanticClusters(rows []semanticVectorRow, coords [][2]float64, assignments []int, paperMap map[string]*Paper) ([]SemanticCluster, map[int]string) {
	maxCluster := 0
	for _, cluster := range assignments {
		if cluster > maxCluster {
			maxCluster = cluster
		}
	}

	termScores := make([]map[string]float64, maxCluster+1)
	clusters := make([]SemanticCluster, maxCluster+1)
	for i := range termScores {
		termScores[i] = map[string]float64{}
		clusters[i].ID = i
	}

	for i, row := range rows {
		cluster := assignments[i]
		if row.Anchor {
			continue
		}
		clusters[cluster].Count++
		clusters[cluster].X += coords[i][0]
		clusters[cluster].Y += coords[i][1]
		paper := paperMap[row.PaperID]
		if paper == nil {
			continue
		}
		addTerms(termScores[cluster], paper.Title, 3)
		addTerms(termScores[cluster], firstN(paper.Abstract, 700), 1)
		addTerms(termScores[cluster], strings.ReplaceAll(paper.Categories, ".", " "), 0.6)
	}

	clusterFrequency := map[string]int{}
	for _, scores := range termScores {
		for term, score := range scores {
			if score > 0 {
				clusterFrequency[term]++
			}
		}
	}

	labels := map[int]string{}
	for i := range clusters {
		if clusters[i].Count > 0 {
			clusters[i].X = roundCoord(clusters[i].X / float64(clusters[i].Count))
			clusters[i].Y = roundCoord(clusters[i].Y / float64(clusters[i].Count))
		}

		terms := make([]semanticTermScore, 0, len(termScores[i]))
		for term, score := range termScores[i] {
			idf := math.Log((float64(len(termScores)) + 1) / float64(clusterFrequency[term]+1))
			if idf < 0.12 {
				idf = 0.12
			}
			terms = append(terms, semanticTermScore{term: term, score: score * idf})
		}
		sort.Slice(terms, func(a, b int) bool {
			if terms[a].score == terms[b].score {
				return terms[a].term < terms[b].term
			}
			return terms[a].score > terms[b].score
		})

		labelTerms := make([]string, 0, 3)
		for _, term := range terms {
			if len(labelTerms) == 3 {
				break
			}
			labelTerms = append(labelTerms, formatMapTerm(term.term))
		}
		if len(labelTerms) == 0 {
			labelTerms = append(labelTerms, fmt.Sprintf("Cluster %d", i+1))
		}
		clusters[i].Label = strings.Join(labelTerms, " + ")
		labels[i] = clusters[i].Label
	}

	return clusters, labels
}

func buildSemanticLinks(rows []semanticVectorRow) []SemanticMapLink {
	type linkKey struct {
		a string
		b string
	}
	linkScores := map[linkKey]float64{}
	addLink := func(i, j int, strength float64) {
		if i == j || strength <= 0 {
			return
		}
		a, b := rows[i].PaperID, rows[j].PaperID
		if a > b {
			a, b = b, a
		}
		key := linkKey{a: a, b: b}
		if strength > linkScores[key] {
			linkScores[key] = strength
		}
	}

	for i := range rows {
		neighborCount := 2
		if rows[i].Anchor {
			neighborCount = 8
		}
		topIndex := make([]int, 0, neighborCount)
		topScore := make([]float64, 0, neighborCount)
		for j := range rows {
			if i == j {
				continue
			}
			score := cosine(rows[i].Vector, rows[j].Vector)
			insertTopNeighbor(&topIndex, &topScore, j, score, neighborCount)
		}
		for idx, j := range topIndex {
			addLink(i, j, topScore[idx])
		}
	}

	links := make([]SemanticMapLink, 0, len(linkScores))
	for key, strength := range linkScores {
		links = append(links, SemanticMapLink{
			Source:   key.a,
			Target:   key.b,
			Strength: roundSimilarity(strength),
		})
	}
	sort.Slice(links, func(i, j int) bool {
		return links[i].Strength > links[j].Strength
	})
	return links
}

func insertTopNeighbor(indices *[]int, scores *[]float64, index int, score float64, limit int) {
	pos := sort.Search(len(*scores), func(i int) bool {
		return (*scores)[i] < score
	})
	if pos >= limit {
		return
	}
	*indices = append(*indices, index)
	*scores = append(*scores, score)
	copy((*indices)[pos+1:], (*indices)[pos:])
	copy((*scores)[pos+1:], (*scores)[pos:])
	(*indices)[pos] = index
	(*scores)[pos] = score
	if len(*indices) > limit {
		*indices = (*indices)[:limit]
		*scores = (*scores)[:limit]
	}
}

func addTerms(scores map[string]float64, text string, weight float64) {
	for _, term := range extractMapTerms(text) {
		scores[term] += weight
	}
}

func extractMapTerms(text string) []string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	terms := make([]string, 0, len(words))
	for _, word := range words {
		word = strings.TrimSpace(word)
		if word == "" || strings.ContainsRune(word, unicode.ReplacementChar) || mapTermStopwords[word] {
			continue
		}
		if !isASCIIMapTerm(word) || isRepeatedMapToken(word) {
			continue
		}
		if len([]rune(word)) < 3 && !mapShortTerms[word] {
			continue
		}
		terms = append(terms, word)
	}
	return terms
}

func isASCIIMapTerm(term string) bool {
	for _, r := range term {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}

func isRepeatedMapToken(term string) bool {
	if len(term) < 6 || len(term)%2 != 0 {
		return false
	}
	unit := term[:2]
	for i := 2; i < len(term); i += 2 {
		if term[i:i+2] != unit {
			return false
		}
	}
	return true
}

func formatMapTerm(term string) string {
	if replacement, ok := mapTermAcronyms[term]; ok {
		return replacement
	}
	runes := []rune(term)
	if len(runes) <= 1 {
		return strings.ToUpper(term)
	}
	return strings.ToUpper(string(runes[:1])) + string(runes[1:])
}

func firstN(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n])
}

func dot(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	total := 0.0
	for i := 0; i < n; i++ {
		total += a[i] * b[i]
	}
	return total
}

func cosine(a, b []float64) float64 {
	aa := dot(a, a)
	bb := dot(b, b)
	if aa <= 0 || bb <= 0 {
		return 0
	}
	return dot(a, b) / math.Sqrt(aa*bb)
}

func normalize(v []float64) float64 {
	norm := math.Sqrt(dot(v, v))
	if norm <= 0 {
		return norm
	}
	for i := range v {
		v[i] /= norm
	}
	return norm
}

func orthogonalize(v []float64, base []float64) {
	if len(base) == 0 {
		return
	}
	projection := dot(v, base)
	n := len(v)
	if len(base) < n {
		n = len(base)
	}
	for i := 0; i < n; i++ {
		v[i] -= projection * base[i]
	}
}

func squaredDistance(a, b [2]float64) float64 {
	dx := a[0] - b[0]
	dy := a[1] - b[1]
	return dx*dx + dy*dy
}

func roundCoord(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func roundSimilarity(v float64) float64 {
	return math.Round(v*1000) / 1000
}

func getPaperIDs(results []SemanticResult) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.PaperID
	}
	return ids
}

func (c *Cache) attachPaperDetails(ctx context.Context, results []SemanticResult) []SemanticResult {
	if len(results) == 0 {
		return []SemanticResult{}
	}

	papers, err := c.GetPapersByIDs(ctx, getPaperIDs(results))
	if err != nil {
		return results
	}

	paperMap := make(map[string]*Paper, len(papers))
	for _, paper := range papers {
		p := paper
		paperMap[paper.ID] = &p
	}

	for i := range results {
		if paper, exists := paperMap[results[i].PaperID]; exists {
			results[i].Paper = paper
		}
	}
	return results
}

func (c *Cache) GetPapersByIDs(ctx context.Context, ids []string) ([]Paper, error) {
	var papers []Paper
	err := c.db.WithContext(ctx).Omit("pdf_text").Where("id IN ?", ids).Find(&papers).Error
	return papers, err
}

var mapTermAcronyms = map[string]string{
	"ai":   "AI",
	"api":  "API",
	"bert": "BERT",
	"ckm":  "CKM",
	"cmb":  "CMB",
	"cnn":  "CNN",
	"cp":   "CP",
	"dna":  "DNA",
	"gan":  "GAN",
	"gpu":  "GPU",
	"hep":  "HEP",
	"ir":   "IR",
	"llm":  "LLM",
	"ml":   "ML",
	"nlp":  "NLP",
	"ode":  "ODE",
	"pde":  "PDE",
	"qcd":  "QCD",
	"qed":  "QED",
	"qft":  "QFT",
	"rna":  "RNA",
	"uv":   "UV",
}

var mapShortTerms = map[string]bool{
	"ai": true,
	"cp": true,
	"ml": true,
}

var mapTermStopwords = map[string]bool{
	"a": true, "about": true, "above": true, "across": true, "after": true, "all": true,
	"also": true, "an": true, "and": true, "are": true, "around": true, "as": true,
	"at": true, "based": true, "be": true, "been": true, "between": true, "both": true,
	"but": true, "by": true, "can": true, "case": true, "cases": true, "consider": true,
	"considered": true, "data": true, "different": true, "does": true, "due": true,
	"dependent": true, "each": true, "effect": true, "effects": true, "for": true, "from": true, "given": true,
	"has": true, "have": true, "having": true, "how": true, "however": true, "if": true,
	"in": true, "into": true, "is": true, "it": true, "its": true, "may": true, "method": true,
	"methods": true, "more": true, "most": true, "new": true, "not": true, "of": true,
	"on": true, "one": true, "or": true, "other": true, "our": true, "paper": true,
	"papers": true, "present": true, "presented": true, "problem": true, "problems": true,
	"provide": true, "results": true, "show": true, "shown": true, "study": true,
	"such": true, "than": true, "that": true, "the": true, "their": true, "these": true,
	"this": true, "through": true, "to": true, "towards": true, "two": true, "under": true,
	"using": true, "via": true, "was": true, "we": true, "where": true, "which": true,
	"while": true, "with": true, "within": true, "without": true,
	"arxiv": true, "text": true, "http": true, "https": true,
	"begin": true, "circ": true, "end": true, "frac": true, "left": true, "leftarrow": true, "mathbb": true,
	"pm0":    true,
	"mathbf": true, "mathcal": true, "mathrm": true, "mathit": true, "right": true,
	"rightarrow": true, "textbf": true, "textit": true,
}
