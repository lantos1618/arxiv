package arxiv

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const oaiBaseURL = "https://export.arxiv.org/oai2"

// OAIClient is an OAI-PMH client for arXiv.
type OAIClient struct {
	client  *http.Client
	baseURL string
}

// NewOAIClient creates a new OAI-PMH client.
func NewOAIClient() *OAIClient {
	return &OAIClient{
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		baseURL: oaiBaseURL,
	}
}

// ListRecords fetches records from arXiv via OAI-PMH.
// If resumptionToken is empty, starts from the beginning with the given params.
// If resumptionToken is non-empty, continues from that point.
func (c *OAIClient) ListRecords(ctx context.Context, set string, from, until time.Time, resumptionToken string) (*OAIResponse, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		response, retryAfter, err := c.listRecordsOnce(ctx, set, from, until, resumptionToken)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if retryAfter < 0 || attempt == 3 {
			break
		}
		if retryAfter == 0 {
			retryAfter = time.Duration(1<<attempt) * time.Second
		}
		if retryAfter > 30*time.Second {
			retryAfter = 30 * time.Second
		}
		timer := time.NewTimer(retryAfter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func (c *OAIClient) listRecordsOnce(ctx context.Context, set string, from, until time.Time, resumptionToken string) (*OAIResponse, time.Duration, error) {
	params := url.Values{}
	params.Set("verb", "ListRecords")

	if resumptionToken != "" {
		params.Set("resumptionToken", resumptionToken)
	} else {
		params.Set("metadataPrefix", "arXiv")
		if set != "" {
			params.Set("set", set)
		}
		if !from.IsZero() {
			params.Set("from", from.Format("2006-01-02"))
		}
		if !until.IsZero() {
			params.Set("until", until.Format("2006-01-02"))
		}
	}

	reqURL := c.baseURL + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
	if err != nil {
		return nil, -1, fmt.Errorf("create request: %w", err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, -1, ctx.Err()
		}
		return nil, 0, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode >= 500 {
		return nil, parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()), fmt.Errorf("temporary OAI status: %s", resp.Status)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, -1, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("read body: %w", err)
	}

	var oaiResp oaiPMHResponse
	if err := xml.Unmarshal(body, &oaiResp); err != nil {
		return nil, 0, fmt.Errorf("parse xml: %w", err)
	}

	if oaiResp.Error.Code != "" {
		return nil, -1, fmt.Errorf("oai error %s: %s", oaiResp.Error.Code, oaiResp.Error.Value)
	}

	result := &OAIResponse{
		ResumptionToken:  oaiResp.ListRecords.ResumptionToken.Value,
		CompleteListSize: oaiResp.ListRecords.ResumptionToken.CompleteListSize,
		Cursor:           oaiResp.ListRecords.ResumptionToken.Cursor,
	}

	for _, rec := range oaiResp.ListRecords.Records {
		if rec.Header.Status == "deleted" {
			if id := oaiPaperID(rec.Header.Identifier); id != "" {
				result.DeletedPaperIDs = append(result.DeletedPaperIDs, id)
			}
			continue
		}
		if strings.TrimSpace(rec.Metadata.ArXiv.ID) == "" {
			continue
		}
		paper := Paper{
			ID:         rec.Metadata.ArXiv.ID,
			Title:      strings.TrimSpace(rec.Metadata.ArXiv.Title),
			Abstract:   strings.TrimSpace(rec.Metadata.ArXiv.Abstract),
			Authors:    formatAuthors(rec.Metadata.ArXiv.Authors),
			Categories: rec.Metadata.ArXiv.Categories,
			Comments:   rec.Metadata.ArXiv.Comments,
			JournalRef: rec.Metadata.ArXiv.JournalRef,
			DOI:        rec.Metadata.ArXiv.DOI,
			License:    rec.Metadata.ArXiv.License,
		}

		if rec.Metadata.ArXiv.Created != "" {
			paper.Created, _ = time.Parse("2006-01-02", rec.Metadata.ArXiv.Created)
		}
		if rec.Metadata.ArXiv.Updated != "" {
			paper.Updated, _ = time.Parse("2006-01-02", rec.Metadata.ArXiv.Updated)
		} else {
			paper.Updated = paper.Created
		}

		result.Papers = append(result.Papers, paper)
	}

	result.RecordCount = len(result.Papers) + len(result.DeletedPaperIDs)
	return result, -1, nil
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if retryAt, err := http.ParseTime(value); err == nil {
		if delay := retryAt.Sub(now); delay > 0 {
			return delay
		}
	}
	return 0
}

func oaiPaperID(identifier string) string {
	const prefix = "oai:arXiv.org:"
	identifier = strings.TrimSpace(identifier)
	if strings.HasPrefix(identifier, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(identifier, prefix))
	}
	return ""
}

// OAIResponse contains the parsed response from an OAI-PMH ListRecords request.
type OAIResponse struct {
	Papers           []Paper
	DeletedPaperIDs  []string
	RecordCount      int
	ResumptionToken  string
	CompleteListSize int
	Cursor           int
}

func formatAuthors(authors []oaiAuthor) string {
	var parts []string
	for _, a := range authors {
		name := a.Forenames + " " + a.Keyname
		if a.Suffix != "" {
			name += " " + a.Suffix
		}
		parts = append(parts, strings.TrimSpace(name))
	}
	return strings.Join(parts, ", ")
}

// XML structures for OAI-PMH parsing

type oaiPMHResponse struct {
	XMLName     xml.Name       `xml:"OAI-PMH"`
	Error       oaiError       `xml:"error"`
	ListRecords oaiListRecords `xml:"ListRecords"`
}

type oaiError struct {
	Code  string `xml:"code,attr"`
	Value string `xml:",chardata"`
}

type oaiListRecords struct {
	Records         []oaiRecord        `xml:"record"`
	ResumptionToken oaiResumptionToken `xml:"resumptionToken"`
}

type oaiResumptionToken struct {
	Value            string `xml:",chardata"`
	CompleteListSize int    `xml:"completeListSize,attr"`
	Cursor           int    `xml:"cursor,attr"`
}

type oaiRecord struct {
	Header   oaiHeader   `xml:"header"`
	Metadata oaiMetadata `xml:"metadata"`
}

type oaiHeader struct {
	Identifier string   `xml:"identifier"`
	Status     string   `xml:"status,attr"`
	Datestamp  string   `xml:"datestamp"`
	SetSpec    []string `xml:"setSpec"`
}

type oaiMetadata struct {
	ArXiv oaiArXiv `xml:"arXiv"`
}

type oaiArXiv struct {
	ID         string      `xml:"id"`
	Created    string      `xml:"created"`
	Updated    string      `xml:"updated"`
	Title      string      `xml:"title"`
	Authors    []oaiAuthor `xml:"authors>author"`
	Categories string      `xml:"categories"`
	Comments   string      `xml:"comments"`
	JournalRef string      `xml:"journal-ref"`
	DOI        string      `xml:"doi"`
	License    string      `xml:"license"`
	Abstract   string      `xml:"abstract"`
}

type oaiAuthor struct {
	Keyname   string `xml:"keyname"`
	Forenames string `xml:"forenames"`
	Suffix    string `xml:"suffix"`
}
