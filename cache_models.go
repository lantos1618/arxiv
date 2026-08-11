package arxiv

import (
	"time"
)

// Paper represents an arXiv paper's metadata.
type Paper struct {
	// ID is the arXiv identifier (e.g., "2301.00001" or "hep-th/9901001")
	ID string `gorm:"primaryKey"`

	// Created is when the paper was first submitted
	Created time.Time `gorm:"index"`

	// Updated is when the paper was last updated
	Updated time.Time `gorm:"index"`

	// Title of the paper
	Title string

	// Abstract of the paper
	Abstract string

	// Authors as a single string (arXiv format)
	Authors string

	// Categories is a space-separated list of arXiv categories
	Categories string `gorm:"index"`

	// Comments from the submitter (e.g., "10 pages, 3 figures")
	Comments string

	// JournalRef is the journal reference if published
	JournalRef string

	// DOI is the Digital Object Identifier if available
	DOI string

	// License URL
	License string

	// PDFPath is the local path to the PDF (if downloaded)
	PDFPath string `gorm:"column:pdf_path" json:"-"`

	// SourcePath is the local path to the TeX source (if downloaded)
	SourcePath string `gorm:"column:src_path" json:"-"`

	// PDFText is extracted text from PDF for search
	PDFText string `gorm:"type:text;column:pdf_text" json:"-"`

	// PDFDownloaded indicates if the PDF has been downloaded
	PDFDownloaded bool `gorm:"column:pdf_downloaded"`

	// SourceDownloaded indicates if the source has been downloaded
	SourceDownloaded bool `gorm:"column:src_downloaded"`

	// MetadataUpdated timestamp
	MetadataUpdated *time.Time `gorm:"column:metadata_updated"`

	// FetchedAt is when the paper was added/fetched to the local cache
	FetchedAt *time.Time `gorm:"column:fetched_at;index"`
}

func (Paper) TableName() string {
	return "papers"
}

// Citation represents a citation relationship between papers.
type Citation struct {
	FromID string `gorm:"primaryKey;column:from_id"`
	ToID   string `gorm:"primaryKey;column:to_id;index"`
}

func (Citation) TableName() string {
	return "citations"
}

// CategoryStat stores precomputed category counts for fast category listings.
type CategoryStat struct {
	Name      string    `gorm:"primaryKey;column:name"`
	Count     int       `gorm:"column:count"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (CategoryStat) TableName() string {
	return "category_counts"
}

// SyncState stores sync metadata.
type SyncState struct {
	Key   string `gorm:"primaryKey"`
	Value string
}

func (SyncState) TableName() string {
	return "sync_state"
}

// DownloadQueueItem is the legacy migration shape for the abandoned download
// queue. New code must not create or report these rows.
type DownloadQueueItem struct {
	PaperID   string `gorm:"primaryKey;column:paper_id"`
	Type      string
	Priority  int
	Added     *time.Time
	Attempts  int
	LastError string `gorm:"column:last_error"`
}

func (DownloadQueueItem) TableName() string {
	return "download_queue"
}

// Embedding stores vector embeddings for semantic search.
// Note: The vector column is handled via raw SQL for pgvector compatibility.
type Embedding struct {
	PaperID string    `gorm:"primaryKey;column:paper_id"`
	Model   string    `gorm:"column:model"`
	Created time.Time `gorm:"column:created"`
	// Vector is managed via raw SQL (pgvector type in PostgreSQL)
}

func (Embedding) TableName() string {
	return "embeddings"
}

// EmbeddingV2 stores next-generation paper-level embeddings.
// Vector is managed via raw SQL because pgvector dimensions are database types.
type EmbeddingV2 struct {
	PaperID       string    `gorm:"primaryKey;column:paper_id"`
	Scope         string    `gorm:"primaryKey;column:scope"` // abstract, title, etc.
	Model         string    `gorm:"primaryKey;column:model"`
	Dim           int       `gorm:"primaryKey;column:dim"`
	SourceHash    string    `gorm:"column:source_hash;index"`
	TextChars     int       `gorm:"column:text_chars"`
	TokenEstimate int       `gorm:"column:token_estimate"`
	Created       time.Time `gorm:"column:created;autoCreateTime"`
	Updated       time.Time `gorm:"column:updated;autoUpdateTime"`
	// Vector is managed via raw SQL (pgvector type in PostgreSQL)
}

func (EmbeddingV2) TableName() string {
	return "embeddings_v2"
}

// PaperChunk stores section/chunk text for full-paper semantic search.
type PaperChunk struct {
	ID            string    `gorm:"primaryKey;column:id"`
	PaperID       string    `gorm:"column:paper_id;index"`
	Scope         string    `gorm:"column:scope;index"` // pdf_text, source, abstract
	Section       string    `gorm:"column:section;index"`
	ChunkIndex    int       `gorm:"column:chunk_index"`
	Text          string    `gorm:"column:text;type:text"`
	TextHash      string    `gorm:"column:text_hash;index"`
	TextChars     int       `gorm:"column:text_chars"`
	TokenEstimate int       `gorm:"column:token_estimate"`
	Created       time.Time `gorm:"column:created;autoCreateTime"`
	Updated       time.Time `gorm:"column:updated;autoUpdateTime"`
}

func (PaperChunk) TableName() string {
	return "paper_chunks"
}

// ChunkEmbeddingV2 stores next-generation full-paper chunk embeddings.
type ChunkEmbeddingV2 struct {
	ChunkID       string    `gorm:"primaryKey;column:chunk_id"`
	Model         string    `gorm:"primaryKey;column:model"`
	Dim           int       `gorm:"primaryKey;column:dim"`
	SourceHash    string    `gorm:"column:source_hash;index"`
	TextChars     int       `gorm:"column:text_chars"`
	TokenEstimate int       `gorm:"column:token_estimate"`
	Created       time.Time `gorm:"column:created;autoCreateTime"`
	Updated       time.Time `gorm:"column:updated;autoUpdateTime"`
	// Vector is managed via raw SQL (pgvector type in PostgreSQL)
}

func (ChunkEmbeddingV2) TableName() string {
	return "chunk_embeddings_v2"
}

// QwenEmbeddingJobStatus represents the lifecycle of a Qwen JIT job.
type QwenEmbeddingJobStatus string

const (
	QwenEmbeddingJobQueued   QwenEmbeddingJobStatus = "queued"
	QwenEmbeddingJobRunning  QwenEmbeddingJobStatus = "running"
	QwenEmbeddingJobComplete QwenEmbeddingJobStatus = "complete"
	QwenEmbeddingJobFailed   QwenEmbeddingJobStatus = "failed"
)

const (
	QwenEmbeddingJobKindAbstract        = "abstract"
	QwenEmbeddingJobKindPaperChunks     = "paper_chunks"
	QwenEmbeddingJobKindChunkEmbeddings = "chunk_embeddings"
	QwenEmbeddingJobKindQuery           = "query"
)

// QwenEmbeddingJob queues the on-demand Qwen work needed for a paper.
type QwenEmbeddingJob struct {
	ID          string                 `gorm:"primaryKey;column:id" json:"id"`
	PaperID     string                 `gorm:"column:paper_id;uniqueIndex:idx_qwen_embedding_jobs_target;index" json:"paperId"`
	Kind        string                 `gorm:"column:kind;uniqueIndex:idx_qwen_embedding_jobs_target;index" json:"kind"`
	Scope       string                 `gorm:"column:scope;uniqueIndex:idx_qwen_embedding_jobs_target;index" json:"scope"`
	Model       string                 `gorm:"column:model;uniqueIndex:idx_qwen_embedding_jobs_target" json:"model"`
	Dim         int                    `gorm:"column:dim;uniqueIndex:idx_qwen_embedding_jobs_target" json:"dim"`
	Status      QwenEmbeddingJobStatus `gorm:"column:status;default:queued;index" json:"status"`
	Priority    int                    `gorm:"column:priority;default:0;index" json:"priority"`
	Attempts    int                    `gorm:"column:attempts;default:0" json:"attempts"`
	LeaseOwner  string                 `gorm:"column:lease_owner;index" json:"leaseOwner,omitempty"`
	LeaseUntil  *time.Time             `gorm:"column:lease_until;index" json:"leaseUntil,omitempty"`
	LastError   string                 `gorm:"column:last_error" json:"lastError,omitempty"`
	CreatedAt   time.Time              `gorm:"column:created_at;autoCreateTime" json:"createdAt"`
	UpdatedAt   time.Time              `gorm:"column:updated_at;autoUpdateTime" json:"updatedAt"`
	CompletedAt *time.Time             `gorm:"column:completed_at;index" json:"completedAt,omitempty"`
}

func (QwenEmbeddingJob) TableName() string {
	return "qwen_embedding_jobs"
}

// QwenQueryEmbedding stores cached query vectors for JIT semantic/deep search.
type QwenQueryEmbedding struct {
	QueryHash     string    `gorm:"primaryKey;column:query_hash"`
	QueryText     string    `gorm:"column:query_text;type:text"`
	Model         string    `gorm:"primaryKey;column:model"`
	Dim           int       `gorm:"primaryKey;column:dim"`
	TextChars     int       `gorm:"column:text_chars"`
	TokenEstimate int       `gorm:"column:token_estimate"`
	Created       time.Time `gorm:"column:created;autoCreateTime"`
	Updated       time.Time `gorm:"column:updated;autoUpdateTime"`
	// Vector is managed via raw SQL (pgvector type in PostgreSQL)
}

func (QwenQueryEmbedding) TableName() string {
	return "qwen_query_embeddings"
}

// EmbeddingJobStatus represents the status of an embedding job.
type EmbeddingJobStatus string

const (
	EmbeddingJobPending    EmbeddingJobStatus = "pending"
	EmbeddingJobProcessing EmbeddingJobStatus = "processing"
	EmbeddingJobCompleted  EmbeddingJobStatus = "completed"
	EmbeddingJobFailed     EmbeddingJobStatus = "failed"
)

// EmbeddingJob represents a queued embedding generation job.
type EmbeddingJob struct {
	PaperID   string             `gorm:"primaryKey;column:paper_id"`
	Status    EmbeddingJobStatus `gorm:"column:status;default:pending;index"`
	Priority  int                `gorm:"column:priority;default:0;index"`
	Attempts  int                `gorm:"column:attempts;default:0"`
	LastError string             `gorm:"column:last_error"`
	CreatedAt time.Time          `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time          `gorm:"column:updated_at;autoUpdateTime"`
}

func (EmbeddingJob) TableName() string {
	return "embedding_jobs"
}

// AuthorCollaboration represents a co-author relationship between two authors.
type AuthorCollaboration struct {
	Author1     string    `gorm:"primaryKey;column:author1;index"`
	Author2     string    `gorm:"primaryKey;column:author2;index"`
	PaperCount  int       `gorm:"column:paper_count"`
	PaperIDs    string    `gorm:"column:paper_ids;type:text"` // JSON array of paper IDs
	FirstCollab time.Time `gorm:"column:first_collab"`
	LastCollab  time.Time `gorm:"column:last_collab"`
}

func (AuthorCollaboration) TableName() string {
	return "author_collaborations"
}

// AuthorEmbedding stores aggregated embeddings for an author.
// Vector is the average of all their paper embeddings.
type AuthorEmbedding struct {
	Author     string    `gorm:"primaryKey;column:author"`
	PaperCount int       `gorm:"column:paper_count"`
	Model      string    `gorm:"column:model"`
	Updated    time.Time `gorm:"column:updated"`
	// Vector is managed via raw SQL (pgvector type in PostgreSQL)
}

func (AuthorEmbedding) TableName() string {
	return "author_embeddings"
}

// User represents a signed-in arXiv.gg account.
type User struct {
	ID            string `gorm:"primaryKey;column:id"`
	Email         string `gorm:"column:email;uniqueIndex"`
	Name          string `gorm:"column:name"`
	PictureURL    string `gorm:"column:picture_url"`
	EmailVerified bool   `gorm:"column:email_verified;default:false"`
	AuthProvider  string `gorm:"column:auth_provider"`
	// Plan is an informational access label. No paid lifecycle or billing state
	// is implemented by the core package.
	Plan        string     `gorm:"column:plan;index;default:free"`
	CreatedAt   time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	LastLoginAt *time.Time `gorm:"column:last_login_at"`
}

func (User) TableName() string {
	return "users"
}

// LoginCode stores short-lived email login codes.
type LoginCode struct {
	ID        string     `gorm:"primaryKey;column:id"`
	Email     string     `gorm:"column:email;index"`
	CodeSalt  string     `gorm:"column:code_salt"`
	CodeHash  string     `gorm:"column:code_hash"`
	Attempts  int        `gorm:"column:attempts;default:0"`
	CreatedAt time.Time  `gorm:"column:created_at;autoCreateTime"`
	ExpiresAt time.Time  `gorm:"column:expires_at;index"`
	UsedAt    *time.Time `gorm:"column:used_at;index"`
}

func (LoginCode) TableName() string {
	return "login_codes"
}

// UserSession stores server-side sessions. Cookies hold only the raw token.
type UserSession struct {
	ID         string    `gorm:"primaryKey;column:id"`
	UserID     string    `gorm:"column:user_id;index"`
	TokenHash  string    `gorm:"column:token_hash;uniqueIndex"`
	UserAgent  string    `gorm:"column:user_agent"`
	IP         string    `gorm:"column:ip"`
	CreatedAt  time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt  time.Time `gorm:"column:updated_at;autoUpdateTime"`
	ExpiresAt  time.Time `gorm:"column:expires_at;index"`
	LastSeenAt time.Time `gorm:"column:last_seen_at"`
}

func (UserSession) TableName() string {
	return "user_sessions"
}

// UserAPIKey stores hashed API keys for signed-in agent and REST access.
type UserAPIKey struct {
	ID         string     `gorm:"primaryKey;column:id"`
	UserID     string     `gorm:"column:user_id;index"`
	Name       string     `gorm:"column:name"`
	KeyHash    string     `gorm:"column:key_hash;uniqueIndex"`
	Prefix     string     `gorm:"column:prefix"`
	LastFour   string     `gorm:"column:last_four"`
	CreatedAt  time.Time  `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt  time.Time  `gorm:"column:updated_at;autoUpdateTime"`
	LastUsedAt *time.Time `gorm:"column:last_used_at;index"`
	RevokedAt  *time.Time `gorm:"column:revoked_at;index"`
}

func (UserAPIKey) TableName() string {
	return "user_api_keys"
}

// UserPaperView stores a compact per-user reading history.
type UserPaperView struct {
	UserID        string    `gorm:"primaryKey;column:user_id"`
	PaperID       string    `gorm:"primaryKey;column:paper_id;index"`
	ViewCount     int64     `gorm:"column:view_count;default:1"`
	FirstViewedAt time.Time `gorm:"column:first_viewed_at;autoCreateTime;index"`
	LastViewedAt  time.Time `gorm:"column:last_viewed_at;autoUpdateTime;index"`
	CreatedAt     time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

func (UserPaperView) TableName() string {
	return "user_paper_views"
}

// FeedbackPost stores community answers to the pricing/product feedback prompt.
type FeedbackPost struct {
	ID          string     `gorm:"primaryKey;column:id"`
	UserID      string     `gorm:"column:user_id;index"`
	Body        string     `gorm:"column:body;type:text"`
	DisplayName string     `gorm:"column:display_name"`
	Anonymous   bool       `gorm:"column:anonymous;default:false;index"`
	OpenToCall  bool       `gorm:"column:open_to_call;default:false;index"`
	Status      string     `gorm:"column:status;default:visible;index"`
	HiddenAt    *time.Time `gorm:"column:hidden_at;index"`
	HiddenBy    string     `gorm:"column:hidden_by"`
	CreatedAt   time.Time  `gorm:"column:created_at;autoCreateTime;index"`
	UpdatedAt   time.Time  `gorm:"column:updated_at;autoUpdateTime"`
}

func (FeedbackPost) TableName() string {
	return "feedback_posts"
}

// FeedbackVote stores one signed-in up/down vote per feedback post.
type FeedbackVote struct {
	UserID    string    `gorm:"primaryKey;column:user_id"`
	PostID    string    `gorm:"primaryKey;column:post_id;index"`
	Value     int       `gorm:"column:value;default:1;index"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
}

func (FeedbackVote) TableName() string {
	return "feedback_votes"
}

// AdminAuditLog records admin page views and moderation actions.
type AdminAuditLog struct {
	ID         string    `gorm:"primaryKey;column:id"`
	AdminEmail string    `gorm:"column:admin_email;index"`
	Action     string    `gorm:"column:action;index"`
	TargetType string    `gorm:"column:target_type;index"`
	TargetID   string    `gorm:"column:target_id;index"`
	Details    string    `gorm:"column:details;type:text"`
	CreatedAt  time.Time `gorm:"column:created_at;autoCreateTime;index"`
}

func (AdminAuditLog) TableName() string {
	return "admin_audit_log"
}
