package arxiv

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const apiBaseURL = "https://export.arxiv.org/api/query"
const absBaseURL = "https://arxiv.org/abs"
const arxivFetchUserAgent = "arxiv.gg metadata fetcher (https://arxiv.gg)"

var (
	validArxivIDPattern    = regexp.MustCompile(`(?i)^(?:\d{4}\.\d{4,6}|(?:acc-phys|adap-org|alg-geom|ao-sci|astro-ph|atom-ph|bayes-an|chao-dyn|chem-ph|cmp-lg|comp-gas|cond-mat|cs|dg-ga|funct-an|gr-qc|hep-ex|hep-lat|hep-ph|hep-th|math|math-ph|mtrl-th|nlin|nucl-ex|nucl-th|patt-sol|physics|plasm-ph|q-alg|quant-ph|solv-int|supr-con)/\d{7})(?:v\d+)?$`)
	metaTagPattern         = regexp.MustCompile(`(?is)<meta\s+[^>]*>`)
	htmlAttributePattern   = regexp.MustCompile(`(?is)([a-z_:.-]+)\s*=\s*("([^"]*)"|'([^']*)'|([^\s"'>/]+))`)
	subjectCellPattern     = regexp.MustCompile(`(?is)<td[^>]*class=["'][^"']*\bsubjects\b[^"']*["'][^>]*>(.*?)</td>`)
	subjectCategoryPattern = regexp.MustCompile(`\(([a-z][a-z-]*(?:\.[A-Za-z-]+)?)\)`)
	duplicateSpacePattern  = regexp.MustCompile(`\s+`)
)

// PartialMetadataError reports IDs omitted from an otherwise valid arXiv batch
// response. Callers receive successfully stored papers alongside this error.
type PartialMetadataError struct {
	MissingIDs []string
}

func (e *PartialMetadataError) Error() string {
	return fmt.Sprintf("metadata response omitted %d requested paper(s): %s", len(e.MissingIDs), strings.Join(e.MissingIDs, ", "))
}

// Fetch retrieves a paper's metadata directly from arXiv API and stores it.
// This is for fetching individual papers without a full OAI-PMH sync.
func (c *Cache) Fetch(ctx context.Context, id string) (*Paper, error) {
	id = strings.TrimSpace(id)
	if !ValidArxivID(id) {
		return nil, fmt.Errorf("invalid arXiv ID")
	}
	paper, err := c.GetPaper(ctx, id)
	if err == nil {
		now := time.Now()
		if err := c.db.WithContext(ctx).Model(&Paper{}).Where("id = ?", id).Update("fetched_at", now).Error; err != nil {
			return nil, fmt.Errorf("update fetched time: %w", err)
		}
		return c.GetPaperFresh(ctx, id)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("get paper: %w", err)
	}

	// Fetch from arXiv API
	paper, err = fetchPaperMetadata(ctx, id)
	if err != nil {
		return nil, err
	}

	// Store in database
	now := time.Now()
	paper.MetadataUpdated = &now
	paper.FetchedAt = &now
	if err := c.upsertFetchedPaper(ctx, paper); err != nil {
		return nil, fmt.Errorf("store paper: %w", err)
	}
	return c.GetPaperFresh(ctx, paper.ID)
}

// FetchMetadataOnly fetches just the metadata (title, authors, abstract) without downloading source.
// This is cheap and fast - good for populating citation titles.
func (c *Cache) FetchMetadataOnly(ctx context.Context, id string) (*Paper, error) {
	return c.Fetch(ctx, id) // Fetch already does metadata-only
}

// FetchBatch fetches metadata for multiple papers in a single API call.
// arXiv API supports up to ~100 IDs per request.
func (c *Cache) FetchBatch(ctx context.Context, ids []string) ([]*Paper, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	// Filter out papers we already have
	var missing []string
	var existing []*Paper
	for _, id := range ids {
		if !ValidArxivID(id) {
			return existing, fmt.Errorf("invalid arXiv ID")
		}
		if paper, err := c.GetPaper(ctx, id); err == nil {
			existing = append(existing, paper)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return existing, fmt.Errorf("get paper %s: %w", id, err)
		} else {
			missing = append(missing, id)
		}
	}

	if len(missing) == 0 {
		return existing, nil
	}

	// Batch fetch from arXiv API (comma-separated IDs)
	values := url.Values{}
	values.Set("id_list", strings.Join(missing, ","))
	values.Set("max_results", fmt.Sprintf("%d", len(missing)))
	requestURL := apiBaseURL + "?" + values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return existing, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return existing, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return existing, fmt.Errorf("http %s", resp.Status)
	}

	body, err := readLimitedBody(resp.Body, 16<<20)
	if err != nil {
		return existing, err
	}

	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return existing, fmt.Errorf("parse xml: %w", err)
	}

	// Store each paper
	returned := make(map[string]bool, len(feed.Entries))
	for _, entry := range feed.Entries {
		paper := parseAtomEntry(entry)
		if paper.ID == "" {
			continue
		}
		returned[paper.ID] = true

		now := time.Now()
		paper.MetadataUpdated = &now
		paper.FetchedAt = &now
		if err := c.upsertFetchedPaper(ctx, paper); err != nil {
			return existing, fmt.Errorf("store paper %s: %w", paper.ID, err)
		}
		stored, err := c.GetPaperFresh(ctx, paper.ID)
		if err != nil {
			return existing, fmt.Errorf("reload paper %s: %w", paper.ID, err)
		}
		existing = append(existing, stored)
	}
	missingReturned := omittedMetadataIDs(missing, returned)
	if len(missingReturned) > 0 {
		return existing, &PartialMetadataError{MissingIDs: missingReturned}
	}

	return existing, nil
}

func omittedMetadataIDs(requested []string, returned map[string]bool) []string {
	omitted := make([]string, 0)
	for _, id := range requested {
		if !returned[id] {
			omitted = append(omitted, id)
		}
	}
	return omitted
}

func (c *Cache) upsertFetchedPaper(ctx context.Context, paper *Paper) error {
	if err := c.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"created", "updated", "title", "abstract", "authors", "categories",
			"comments", "journal_ref", "doi", "license", "metadata_updated", "fetched_at",
		}),
	}).Create(paper).Error; err != nil {
		return err
	}
	c.paperLRU.Delete(paper.ID)
	c.detailLRU.Clear()
	return nil
}

// PrefetchReferenceTitles fetches metadata for all uncached references of a paper.
// This populates titles without downloading full sources.
func (c *Cache) PrefetchReferenceTitles(ctx context.Context, paperID string) error {
	refs, err := c.References(ctx, paperID)
	if err != nil {
		return err
	}

	var uncached []string
	for _, r := range refs {
		if !r.HasTitle {
			uncached = append(uncached, r.ID)
		}
	}

	if len(uncached) == 0 {
		return nil
	}

	// Batch fetch in chunks of 50 (arXiv limit is ~100)
	var fetchErrors []error
	for i := 0; i < len(uncached); i += 50 {
		end := i + 50
		if end > len(uncached) {
			end = len(uncached)
		}
		chunk := uncached[i:end]

		if _, err := c.FetchBatch(ctx, chunk); err != nil {
			fetchErrors = append(fetchErrors, err)
		}

		// Rate limit between batches
		if end < len(uncached) {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return errors.Join(append(fetchErrors, ctx.Err())...)
			case <-timer.C:
			}
		}
	}

	return errors.Join(fetchErrors...)
}

// FetchAndDownload fetches metadata and downloads source/PDF for a paper.
func (c *Cache) FetchAndDownload(ctx context.Context, id string, opts *DownloadOptions) (*Paper, error) {
	paper, err := c.Fetch(ctx, id)
	if err != nil {
		return nil, err
	}

	if err := c.DownloadPaper(ctx, id, opts); err != nil {
		return paper, fmt.Errorf("download: %w", err)
	}

	paper, err = c.GetPaper(ctx, id)
	if err != nil {
		return paper, err
	}

	return paper, nil
}

func fetchPaperMetadata(ctx context.Context, id string) (*Paper, error) {
	paper, err := fetchPaperMetadataAtom(ctx, id)
	if err == nil {
		return paper, nil
	}

	fallbackPaper, fallbackErr := fetchPaperMetadataHTML(ctx, id)
	if fallbackErr == nil {
		return fallbackPaper, nil
	}

	return nil, fmt.Errorf("%w; html fallback: %v", err, fallbackErr)
}

func fetchPaperMetadataAtom(ctx context.Context, id string) (*Paper, error) {
	if !ValidArxivID(id) {
		return nil, fmt.Errorf("invalid arXiv ID")
	}
	values := url.Values{}
	values.Set("id_list", id)
	requestURL := apiBaseURL + "?" + values.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", arxivFetchUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %s", resp.Status)
	}

	body, err := readLimitedBody(resp.Body, 2<<20)
	if err != nil {
		return nil, err
	}

	var feed atomFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, fmt.Errorf("parse xml: %w", err)
	}

	if len(feed.Entries) == 0 {
		return nil, fmt.Errorf("paper not found: %s", id)
	}

	entry := feed.Entries[0]

	// Extract ID from the URL (e.g., http://arxiv.org/abs/2301.00001v1 -> 2301.00001)
	paperID := id
	if idx := strings.LastIndex(entry.ID, "/abs/"); idx >= 0 {
		paperID = entry.ID[idx+5:]
		// Remove version suffix
		if vIdx := strings.LastIndex(paperID, "v"); vIdx > 0 {
			paperID = paperID[:vIdx]
		}
	}

	var authors []string
	for _, a := range entry.Authors {
		authors = append(authors, a.Name)
	}

	var categories []string
	for _, c := range entry.Categories {
		categories = append(categories, c.Term)
	}

	paper := &Paper{
		ID:         paperID,
		Title:      strings.TrimSpace(entry.Title),
		Abstract:   strings.TrimSpace(entry.Summary),
		Authors:    strings.Join(authors, ", "),
		Categories: strings.Join(categories, " "),
		Comments:   entry.Comment,
		JournalRef: entry.JournalRef,
		DOI:        entry.DOI,
	}

	paper.Created, _ = time.Parse(time.RFC3339, entry.Published)
	paper.Updated, _ = time.Parse(time.RFC3339, entry.Updated)

	return paper, nil
}

func fetchPaperMetadataHTML(ctx context.Context, id string) (*Paper, error) {
	if !ValidArxivID(id) {
		return nil, fmt.Errorf("invalid arXiv ID")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "GET", absBaseURL+"/"+id, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", arxivFetchUserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %s", resp.Status)
	}

	body, err := readLimitedBody(resp.Body, 2<<20)
	if err != nil {
		return nil, err
	}

	paper, err := parseArxivHTMLMetadata(id, string(body))
	if err != nil {
		return nil, err
	}
	return paper, nil
}

func ValidArxivID(id string) bool {
	return validArxivIDPattern.MatchString(strings.TrimSpace(id))
}

func readLimitedBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return body, nil
}

func parseArxivHTMLMetadata(requestedID, body string) (*Paper, error) {
	metadata := extractCitationMetadata(body)

	paperID := strings.TrimSpace(firstMetadataValue(metadata, "citation_arxiv_id"))
	if paperID == "" {
		paperID = requestedID
	}

	title := cleanHTMLMetadataText(firstMetadataValue(metadata, "citation_title"))
	abstract := cleanHTMLMetadataText(firstMetadataValue(metadata, "citation_abstract"))
	if title == "" || abstract == "" {
		return nil, fmt.Errorf("missing citation metadata for %s", requestedID)
	}

	authors := metadata["citation_author"]
	for i := range authors {
		authors[i] = normalizeCitationAuthor(cleanHTMLMetadataText(authors[i]))
	}

	paper := &Paper{
		ID:         paperID,
		Title:      title,
		Abstract:   abstract,
		Authors:    strings.Join(authors, ", "),
		Categories: strings.Join(extractHTMLCategories(body), " "),
		DOI:        cleanHTMLMetadataText(firstMetadataValue(metadata, "citation_doi")),
	}

	if dateValue := firstMetadataValue(metadata, "citation_date"); dateValue != "" {
		if created, err := time.Parse("2006/01/02", dateValue); err == nil {
			paper.Created = created
			paper.Updated = created
		}
	}
	if dateValue := firstMetadataValue(metadata, "citation_online_date"); dateValue != "" {
		if updated, err := time.Parse("2006/01/02", dateValue); err == nil {
			paper.Updated = updated
			if paper.Created.IsZero() {
				paper.Created = updated
			}
		}
	}

	return paper, nil
}

func extractCitationMetadata(body string) map[string][]string {
	metadata := make(map[string][]string)
	for _, tag := range metaTagPattern.FindAllString(body, -1) {
		attrs := extractHTMLAttributes(tag)
		name := strings.ToLower(attrs["name"])
		if !strings.HasPrefix(name, "citation_") {
			continue
		}
		metadata[name] = append(metadata[name], attrs["content"])
	}
	return metadata
}

func extractHTMLAttributes(tag string) map[string]string {
	attrs := make(map[string]string)
	for _, match := range htmlAttributePattern.FindAllStringSubmatch(tag, -1) {
		if len(match) < 6 {
			continue
		}
		value := match[3]
		if value == "" {
			value = match[4]
		}
		if value == "" {
			value = match[5]
		}
		attrs[strings.ToLower(match[1])] = html.UnescapeString(value)
	}
	return attrs
}

func extractHTMLCategories(body string) []string {
	match := subjectCellPattern.FindStringSubmatch(body)
	if len(match) < 2 {
		return nil
	}

	seen := make(map[string]bool)
	var categories []string
	for _, categoryMatch := range subjectCategoryPattern.FindAllStringSubmatch(match[1], -1) {
		if len(categoryMatch) < 2 {
			continue
		}
		category := categoryMatch[1]
		if !seen[category] {
			seen[category] = true
			categories = append(categories, category)
		}
	}
	return categories
}

func firstMetadataValue(metadata map[string][]string, key string) string {
	values := metadata[strings.ToLower(key)]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func normalizeCitationAuthor(author string) string {
	parts := strings.Split(author, ",")
	if len(parts) != 2 {
		return author
	}
	first := strings.TrimSpace(parts[1])
	last := strings.TrimSpace(parts[0])
	if first == "" || last == "" {
		return author
	}
	return first + " " + last
}

func cleanHTMLMetadataText(value string) string {
	value = html.UnescapeString(value)
	value = strings.TrimSpace(value)
	return duplicateSpacePattern.ReplaceAllString(value, " ")
}

// Atom feed structures for arXiv API

type atomFeed struct {
	XMLName xml.Name    `xml:"feed"`
	Entries []atomEntry `xml:"entry"`
}

type atomEntry struct {
	ID         string         `xml:"id"`
	Title      string         `xml:"title"`
	Summary    string         `xml:"summary"`
	Authors    []atomAuthor   `xml:"author"`
	Categories []atomCategory `xml:"category"`
	Published  string         `xml:"published"`
	Updated    string         `xml:"updated"`
	Comment    string         `xml:"comment"`
	JournalRef string         `xml:"journal_ref"`
	DOI        string         `xml:"doi"`
}

type atomAuthor struct {
	Name string `xml:"name"`
}

type atomCategory struct {
	Term string `xml:"term,attr"`
}

// parseAtomEntry converts an atom entry to a Paper.
func parseAtomEntry(entry atomEntry) *Paper {
	// Extract ID from the URL (e.g., http://arxiv.org/abs/2301.00001v1 -> 2301.00001)
	paperID := ""
	if idx := strings.LastIndex(entry.ID, "/abs/"); idx >= 0 {
		paperID = entry.ID[idx+5:]
		// Remove version suffix
		if vIdx := strings.LastIndex(paperID, "v"); vIdx > 0 {
			paperID = paperID[:vIdx]
		}
	}

	var authors []string
	for _, a := range entry.Authors {
		authors = append(authors, a.Name)
	}

	var categories []string
	for _, c := range entry.Categories {
		categories = append(categories, c.Term)
	}

	paper := &Paper{
		ID:         paperID,
		Title:      strings.TrimSpace(entry.Title),
		Abstract:   strings.TrimSpace(entry.Summary),
		Authors:    strings.Join(authors, ", "),
		Categories: strings.Join(categories, " "),
		Comments:   entry.Comment,
		JournalRef: entry.JournalRef,
		DOI:        entry.DOI,
	}

	paper.Created, _ = time.Parse(time.RFC3339, entry.Published)
	paper.Updated, _ = time.Parse(time.RFC3339, entry.Updated)

	return paper
}
