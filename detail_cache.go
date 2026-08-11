package arxiv

import (
	"context"
	"strings"
	"time"
)

const (
	authorCountTTL   = 30 * time.Minute
	authorSearchTTL  = 30 * time.Minute
	authorProfileTTL = 30 * time.Minute
	authorStatsTTL   = 30 * time.Minute
	citationTTL      = 30 * time.Minute
)

type detailCacheItem struct {
	expires time.Time
	value   interface{}
}

func newSemaphore(size int) chan struct{} {
	if size <= 0 {
		return nil
	}
	return make(chan struct{}, size)
}

func detailKey(parts ...string) string {
	for i, part := range parts {
		parts[i] = strings.ToLower(strings.TrimSpace(part))
	}
	return strings.Join(parts, "\x00")
}

func (c *Cache) getDetailCache(key string) (interface{}, bool) {
	if c.detailLRU == nil {
		return nil, false
	}
	value, ok := c.detailLRU.Get(key)
	if !ok {
		return nil, false
	}
	item, ok := value.(detailCacheItem)
	if !ok || time.Now().After(item.expires) {
		c.detailLRU.Delete(key)
		return nil, false
	}
	return item.value, true
}

func (c *Cache) putDetailCache(key string, ttl time.Duration, value interface{}) {
	if c.detailLRU == nil || ttl <= 0 {
		return
	}
	c.detailLRU.Put(key, detailCacheItem{
		expires: time.Now().Add(ttl),
		value:   value,
	})
}

func (c *Cache) withAuthorQuery(ctx context.Context, fn func() error) error {
	return withQuerySemaphore(ctx, c.authorQuerySem, fn)
}

func (c *Cache) withCitationQuery(ctx context.Context, fn func() error) error {
	return withQuerySemaphore(ctx, c.citationQuerySem, fn)
}

func withQuerySemaphore(ctx context.Context, sem chan struct{}, fn func() error) error {
	if sem == nil {
		return fn()
	}
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func clonePapers(papers []Paper) []Paper {
	out := make([]Paper, len(papers))
	copy(out, papers)
	return out
}

func cloneCollaborators(collabs []CollaboratorInfo) []CollaboratorInfo {
	out := make([]CollaboratorInfo, len(collabs))
	for i, collab := range collabs {
		out[i] = collab
		out[i].PaperIDs = append([]string(nil), collab.PaperIDs...)
	}
	return out
}

func cloneCitingPapers(papers []CitingPaper) []CitingPaper {
	out := make([]CitingPaper, len(papers))
	copy(out, papers)
	return out
}

func cloneReferences(refs []Reference) []Reference {
	out := make([]Reference, len(refs))
	copy(out, refs)
	return out
}

func cloneAuthorStats(stats *AuthorStats) *AuthorStats {
	if stats == nil {
		return nil
	}
	out := *stats
	return &out
}

func cloneAuthorProfile(profile *AuthorProfile) *AuthorProfile {
	if profile == nil {
		return nil
	}
	out := *profile
	out.ResearchAreas = append([]ResearchArea(nil), profile.ResearchAreas...)
	out.YearlyOutput = append([]YearlyOutput(nil), profile.YearlyOutput...)
	if profile.FirstPaper != nil {
		first := *profile.FirstPaper
		out.FirstPaper = &first
	}
	if profile.LastPaper != nil {
		last := *profile.LastPaper
		out.LastPaper = &last
	}
	return &out
}

func cloneAuthorGraph(graph *AuthorGraph) *AuthorGraph {
	if graph == nil {
		return nil
	}
	out := *graph
	out.Nodes = append([]CollabGraphNode(nil), graph.Nodes...)
	out.Edges = append([]CollabGraphEdge(nil), graph.Edges...)
	return &out
}
