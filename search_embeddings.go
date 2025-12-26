package arxiv

import (
	"context"
	"math"
	"sort"

	"github.com/viterin/vek"
)

type SemanticResult struct {
	PaperID    string  `json:"paperId"`
	Similarity float64 `json:"similarity"`
	Paper      *Paper  `json:"paper,omitempty"`
}

func (c *Cache) SearchSemantic(ctx context.Context, queryEmbedding []float32, limit int) ([]SemanticResult, error) {
	if limit <= 0 {
		limit = 20
	}

	var embeddings []Embedding
	err := c.db.WithContext(ctx).Find(&embeddings).Error
	if err != nil {
		return nil, err
	}

	if len(embeddings) == 0 {
		return []SemanticResult{}, nil
	}

	results := make([]SemanticResult, 0, len(embeddings))

	for _, emb := range embeddings {
		vector := bytesToFloat32Slice(emb.Vector)
		if len(vector) != len(queryEmbedding) {
			continue
		}

		similarity := cosineSimilarity(queryEmbedding, vector)
		if similarity > 0 {
			results = append(results, SemanticResult{
				PaperID:    emb.PaperID,
				Similarity: similarity,
			})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})

	if len(results) > limit {
		results = results[:limit]
	}

	papers, err := c.GetPapersByIDs(ctx, getPaperIDs(results))
	if err == nil {
		paperMap := make(map[string]*Paper)
		for _, paper := range papers {
			paperMap[paper.ID] = &paper
		}

		for i := range results {
			if paper, exists := paperMap[results[i].PaperID]; exists {
				results[i].Paper = paper
			}
		}
	}

	return results, nil
}

func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}

	// Convert to float64 for vek library
	a64 := make([]float64, len(a))
	b64 := make([]float64, len(b))
	for i := range a {
		a64[i] = float64(a[i])
		b64[i] = float64(b[i])
	}

	similarity := vek.CosineSimilarity(a64, b64)
	return similarity
}

func bytesToFloat32Slice(data []byte) []float32 {
	if len(data)%4 != 0 {
		return nil
	}

	result := make([]float32, 0, len(data)/4)
	for i := 0; i < len(data); i += 4 {
		bits := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
		result = append(result, math.Float32frombits(bits))
	}
	return result
}

func getPaperIDs(results []SemanticResult) []string {
	ids := make([]string, len(results))
	for i, result := range results {
		ids[i] = result.PaperID
	}
	return ids
}

func (c *Cache) GetPapersByIDs(ctx context.Context, ids []string) ([]Paper, error) {
	var papers []Paper
	err := c.db.WithContext(ctx).Where("id IN ?", ids).Find(&papers).Error
	return papers, err
}
