// Package main provides a migration tool from SQLite to PostgreSQL.
package main

import (
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"

	"github.com/lantos1618/arxiv.gg"
)

type migrationConfig struct {
	sqlitePath  string
	postgresURL string
	batchSize   int
}

type modelMigration struct {
	name       string
	model      any
	primaryKey []string
	migrate    func(context.Context, *gorm.DB, *gorm.DB, int) error
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runMigration(ctx, os.Args[1:], os.Getenv); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func runMigration(ctx context.Context, args []string, getenv func(string) string) error {
	config, err := parseMigrationConfig(args, getenv)
	if err != nil {
		return err
	}
	info, err := os.Stat(config.sqlitePath)
	if err != nil {
		return fmt.Errorf("open SQLite source %q: %w", config.sqlitePath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("SQLite source %q is a directory", config.sqlitePath)
	}

	log.Printf("Opening SQLite database: %s", config.sqlitePath)
	src, err := gorm.Open(sqlite.Open(config.sqlitePath), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return fmt.Errorf("open SQLite: %w", err)
	}
	if err := closeDatabaseOnReturn(src); err != nil {
		return err
	}
	defer closeDatabase(src)

	log.Print("Connecting to PostgreSQL...")
	dst, err := gorm.Open(postgres.Open(config.postgresURL), &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	dstSQL, err := dst.DB()
	if err != nil {
		return fmt.Errorf("configure PostgreSQL pool: %w", err)
	}
	defer dstSQL.Close()
	dstSQL.SetMaxIdleConns(10)
	dstSQL.SetMaxOpenConns(50)

	log.Print("Running destination schema migrations...")
	models := currentModels()
	modelValues := make([]any, 0, len(models))
	for _, migration := range models {
		modelValues = append(modelValues, migration.model)
	}
	if err := dst.WithContext(ctx).AutoMigrate(modelValues...); err != nil {
		return fmt.Errorf("migrate destination schema: %w", err)
	}
	if err := initPostgresSchema(dst.WithContext(ctx)); err != nil {
		return fmt.Errorf("initialize PostgreSQL schema: %w", err)
	}

	migratedTables := 0
	skippedTables := 0
	for _, migration := range models {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("migration canceled: %w", err)
		}
		if !src.Migrator().HasTable(migration.model) {
			log.Printf("Skipping %s: source table is not present", migration.name)
			skippedTables++
			continue
		}
		log.Printf("Migrating %s...", migration.name)
		if err := migration.migrate(ctx, src, dst, config.batchSize); err != nil {
			return fmt.Errorf("migrate %s: %w", migration.name, err)
		}
		if src.Migrator().HasColumn(migration.model, "vector") {
			if err := migrateVectorColumn(ctx, src, dst, migration.model, migration.primaryKey, config.batchSize); err != nil {
				return fmt.Errorf("migrate %s vectors: %w", migration.name, err)
			}
		}
		migratedTables++
	}

	log.Print("Building missing full-text search vectors...")
	if err := rebuildSearchIndex(ctx, dst); err != nil {
		return fmt.Errorf("rebuild search index: %w", err)
	}
	log.Printf("Migration verified: %d source tables copied and checked; %d absent source tables skipped", migratedTables, skippedTables)
	return nil
}

func parseMigrationConfig(args []string, getenv func(string) string) (migrationConfig, error) {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	sqlitePath := fs.String("sqlite", "/data/arxiv/index.db", "SQLite database path")
	postgresURL := fs.String("postgres", "", "PostgreSQL connection URL (or use DATABASE_URL env)")
	batchSize := fs.Int("batch", 500, "Rows per deterministic migration batch")
	if err := fs.Parse(args); err != nil {
		return migrationConfig{}, err
	}
	if fs.NArg() != 0 {
		return migrationConfig{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *batchSize <= 0 {
		return migrationConfig{}, fmt.Errorf("batch size must be greater than zero")
	}
	url := strings.TrimSpace(*postgresURL)
	if url == "" {
		url = strings.TrimSpace(getenv("DATABASE_URL"))
	}
	if url == "" {
		return migrationConfig{}, fmt.Errorf("PostgreSQL URL required: use -postgres or DATABASE_URL")
	}
	if strings.TrimSpace(*sqlitePath) == "" {
		return migrationConfig{}, fmt.Errorf("SQLite database path must not be empty")
	}
	return migrationConfig{sqlitePath: *sqlitePath, postgresURL: url, batchSize: *batchSize}, nil
}

func closeDatabaseOnReturn(db *gorm.DB) error {
	_, err := db.DB()
	if err != nil {
		return fmt.Errorf("access database connection: %w", err)
	}
	return nil
}

func closeDatabase(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err == nil {
		_ = sqlDB.Close()
	}
}

func currentModels() []modelMigration {
	return []modelMigration{
		migrationFor("papers", &arxiv.Paper{}, []string{"id"}),
		migrationFor("citations", &arxiv.Citation{}, []string{"from_id", "to_id"}),
		migrationFor("category counts", &arxiv.CategoryStat{}, []string{"name"}),
		migrationFor("sync state", &arxiv.SyncState{}, []string{"key"}),
		migrationFor("download queue", &arxiv.DownloadQueueItem{}, []string{"paper_id"}),
		migrationFor("embeddings", &arxiv.Embedding{}, []string{"paper_id"}),
		migrationFor("v2 paper embeddings", &arxiv.EmbeddingV2{}, []string{"paper_id", "scope", "model", "dim"}),
		migrationFor("paper chunks", &arxiv.PaperChunk{}, []string{"id"}),
		migrationFor("v2 chunk embeddings", &arxiv.ChunkEmbeddingV2{}, []string{"chunk_id", "model", "dim"}),
		migrationFor("Qwen embedding jobs", &arxiv.QwenEmbeddingJob{}, []string{"id"}),
		migrationFor("Qwen query embeddings", &arxiv.QwenQueryEmbedding{}, []string{"query_hash", "model", "dim"}),
		migrationFor("embedding jobs", &arxiv.EmbeddingJob{}, []string{"paper_id"}),
		migrationFor("author collaborations", &arxiv.AuthorCollaboration{}, []string{"author1", "author2"}),
		migrationFor("author embeddings", &arxiv.AuthorEmbedding{}, []string{"author"}),
		migrationFor("users", &arxiv.User{}, []string{"id"}),
		migrationFor("login codes", &arxiv.LoginCode{}, []string{"id"}),
		migrationFor("user sessions", &arxiv.UserSession{}, []string{"id"}),
		migrationFor("user API keys", &arxiv.UserAPIKey{}, []string{"id"}),
		migrationFor("user paper views", &arxiv.UserPaperView{}, []string{"user_id", "paper_id"}),
		migrationFor("feedback posts", &arxiv.FeedbackPost{}, []string{"id"}),
		migrationFor("feedback votes", &arxiv.FeedbackVote{}, []string{"user_id", "post_id"}),
		migrationFor("admin audit log", &arxiv.AdminAuditLog{}, []string{"id"}),
	}
}

func migrationFor[T any](name string, model *T, primaryKey []string) modelMigration {
	return modelMigration{
		name:       name,
		model:      model,
		primaryKey: primaryKey,
		migrate: func(ctx context.Context, src, dst *gorm.DB, batchSize int) error {
			return migrateRows[T](ctx, src, dst, batchSize)
		},
	}
}

func migrateRows[T any](ctx context.Context, src, dst *gorm.DB, batchSize int) error {
	var model T
	var total int64
	if err := src.WithContext(ctx).Model(&model).Count(&total).Error; err != nil {
		return fmt.Errorf("count source rows: %w", err)
	}
	order, err := primaryKeyOrder(src, &model)
	if err != nil {
		return err
	}

	processed := int64(0)
	for offset := 0; ; offset += batchSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		var rows []T
		query := src.WithContext(ctx).Order(order).Offset(offset).Limit(batchSize).Find(&rows)
		if query.Error != nil {
			return fmt.Errorf("read source batch at offset %d: %w", offset, query.Error)
		}
		if len(rows) == 0 {
			break
		}
		if err := dst.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).CreateInBatches(rows, batchSize).Error; err != nil {
			return fmt.Errorf("upsert batch at offset %d: %w", offset, err)
		}
		if err := verifyBatchKeys(ctx, dst, &model, rows); err != nil {
			return fmt.Errorf("verify batch at offset %d: %w", offset, err)
		}
		processed += int64(len(rows))
		log.Printf("  Processed %d/%d rows", processed, total)
	}
	if processed != total {
		return fmt.Errorf("source changed during migration: counted %d rows but processed %d", total, processed)
	}
	var destinationTotal int64
	if err := dst.WithContext(ctx).Model(&model).Count(&destinationTotal).Error; err != nil {
		return fmt.Errorf("count destination rows: %w", err)
	}
	if destinationTotal < total {
		return fmt.Errorf("verification failed: source has %d rows, destination has %d", total, destinationTotal)
	}
	log.Printf("  Verified %d source rows (%d total destination rows)", total, destinationTotal)
	return nil
}

func verifyBatchKeys[T any](ctx context.Context, dst *gorm.DB, model *T, rows []T) error {
	statement := &gorm.Statement{DB: dst}
	if err := statement.Parse(model); err != nil {
		return err
	}
	groups := make([]string, 0, len(rows))
	args := make([]any, 0, len(rows)*len(statement.Schema.PrimaryFields))
	for rowIndex := range rows {
		conditions := make([]string, 0, len(statement.Schema.PrimaryFields))
		value := reflect.ValueOf(&rows[rowIndex])
		for _, field := range statement.Schema.PrimaryFields {
			fieldValue, _ := field.ValueOf(ctx, value)
			conditions = append(conditions, field.DBName+" = ?")
			args = append(args, fieldValue)
		}
		groups = append(groups, "("+strings.Join(conditions, " AND ")+")")
	}
	var found int64
	if err := dst.WithContext(ctx).Model(model).Where(strings.Join(groups, " OR "), args...).Count(&found).Error; err != nil {
		return err
	}
	if found != int64(len(rows)) {
		return fmt.Errorf("found %d of %d source primary keys at destination", found, len(rows))
	}
	return nil
}

func primaryKeyOrder(db *gorm.DB, model any) (string, error) {
	statement := &gorm.Statement{DB: db}
	if err := statement.Parse(model); err != nil {
		return "", fmt.Errorf("inspect model primary key: %w", err)
	}
	columns := make([]string, 0, len(statement.Schema.PrimaryFields))
	for _, field := range statement.Schema.PrimaryFields {
		columns = append(columns, field.DBName)
	}
	if len(columns) == 0 {
		return "", fmt.Errorf("model %s has no primary key", statement.Schema.Table)
	}
	return strings.Join(columns, ", "), nil
}

func migratePapers(ctx context.Context, src, dst *gorm.DB, batchSize int) error {
	return migrateRows[arxiv.Paper](ctx, src, dst, batchSize)
}

func migrateCitations(ctx context.Context, src, dst *gorm.DB, batchSize int) error {
	return migrateRows[arxiv.Citation](ctx, src, dst, batchSize)
}

func migrateEmbeddings(ctx context.Context, src, dst *gorm.DB, batchSize int) error {
	if err := migrateRows[arxiv.Embedding](ctx, src, dst, batchSize); err != nil {
		return err
	}
	if src.Migrator().HasColumn(&arxiv.Embedding{}, "vector") {
		return migrateVectorColumn(ctx, src, dst, &arxiv.Embedding{}, []string{"paper_id"}, batchSize)
	}
	return nil
}

func migrateSyncState(ctx context.Context, src, dst *gorm.DB) error {
	return migrateRows[arxiv.SyncState](ctx, src, dst, 500)
}

func migrateVectorColumn(ctx context.Context, src, dst *gorm.DB, model any, primaryKeys []string, batchSize int) error {
	statement := &gorm.Statement{DB: src}
	if err := statement.Parse(model); err != nil {
		return err
	}
	table := statement.Schema.Table
	quotedKeys := make([]string, len(primaryKeys))
	for i, key := range primaryKeys {
		quotedKeys[i] = `"` + key + `"`
	}
	selectColumns := append(append([]string{}, quotedKeys...), `"vector"`)
	processed := 0
	for offset := 0; ; offset += batchSize {
		rows, err := src.WithContext(ctx).Raw(
			fmt.Sprintf(`SELECT %s FROM "%s" ORDER BY %s LIMIT ? OFFSET ?`, strings.Join(selectColumns, ", "), table, strings.Join(quotedKeys, ", ")),
			batchSize, offset,
		).Rows()
		if err != nil {
			return err
		}
		batchRows := 0
		for rows.Next() {
			values := make([]any, len(primaryKeys)+1)
			pointers := make([]any, len(values))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				return err
			}
			query := fmt.Sprintf(`UPDATE "%s" SET "vector" = ? WHERE `, table)
			args := []any{values[len(values)-1]}
			if dst.Dialector.Name() == "postgres" {
				vector, err := postgresVector(values[len(values)-1])
				if err != nil {
					rows.Close()
					return fmt.Errorf("convert vector at source offset %d: %w", offset+batchRows, err)
				}
				query = fmt.Sprintf(`UPDATE "%s" SET "vector" = CAST(? AS vector) WHERE `, table)
				args[0] = vector
			}
			conditions := make([]string, len(primaryKeys))
			for i, key := range primaryKeys {
				conditions[i] = `"` + key + `" = ?`
				args = append(args, values[i])
			}
			result := dst.WithContext(ctx).Exec(query+strings.Join(conditions, " AND "), args...)
			if result.Error != nil {
				rows.Close()
				return result.Error
			}
			if result.RowsAffected != 1 {
				rows.Close()
				return fmt.Errorf("updated %d destination rows for source vector, want 1", result.RowsAffected)
			}
			batchRows++
			processed++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if batchRows == 0 {
			break
		}
	}
	log.Printf("  Migrated %d vector values", processed)
	return nil
}

func postgresVector(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	data, ok := value.([]byte)
	if !ok {
		return nil, fmt.Errorf("unsupported vector value %T", value)
	}
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "[") {
		return trimmed, nil
	}
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("binary vector has %d bytes, not a multiple of four", len(data))
	}
	parts := make([]string, len(data)/4)
	for i := range parts {
		bits := binary.LittleEndian.Uint32(data[i*4 : i*4+4])
		parts[i] = strconv.FormatFloat(float64(math.Float32frombits(bits)), 'g', -1, 32)
	}
	return "[" + strings.Join(parts, ",") + "]", nil
}

func initPostgresSchema(db *gorm.DB) error {
	statements := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`CREATE INDEX IF NOT EXISTS idx_papers_src_downloaded ON papers(src_downloaded)`,
		`CREATE INDEX IF NOT EXISTS idx_papers_pdf_downloaded ON papers(pdf_downloaded)`,
		`CREATE INDEX IF NOT EXISTS idx_papers_fetched_at ON papers(fetched_at DESC NULLS LAST)`,
		`CREATE INDEX IF NOT EXISTS idx_papers_src_fetched ON papers(src_downloaded, fetched_at DESC NULLS LAST)`,
		`CREATE INDEX IF NOT EXISTS idx_citations_to_id ON citations(to_id)`,
		`CREATE INDEX IF NOT EXISTS idx_citations_from_id ON citations(from_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_user_api_keys_active_name ON user_api_keys(user_id, name) WHERE revoked_at IS NULL`,
		`ALTER TABLE embeddings ADD COLUMN IF NOT EXISTS vector vector(384)`,
		`ALTER TABLE embeddings_v2 ADD COLUMN IF NOT EXISTS vector vector(1024)`,
		`ALTER TABLE chunk_embeddings_v2 ADD COLUMN IF NOT EXISTS vector vector(1024)`,
		`ALTER TABLE qwen_query_embeddings ADD COLUMN IF NOT EXISTS vector vector(1024)`,
		`ALTER TABLE author_embeddings ADD COLUMN IF NOT EXISTS vector vector(384)`,
		`CREATE INDEX IF NOT EXISTS idx_embeddings_vector_hnsw ON embeddings USING hnsw (vector vector_cosine_ops)`,
		`CREATE INDEX IF NOT EXISTS idx_embeddings_v2_lookup ON embeddings_v2(scope, model, dim, paper_id)`,
		`CREATE INDEX IF NOT EXISTS idx_chunk_embeddings_v2_lookup ON chunk_embeddings_v2(model, dim, chunk_id)`,
		`CREATE INDEX IF NOT EXISTS idx_qwen_query_embeddings_lookup ON qwen_query_embeddings(query_hash, model, dim)`,
		`CREATE INDEX IF NOT EXISTS idx_author_embeddings_vector_hnsw ON author_embeddings USING hnsw (vector vector_cosine_ops)`,
		`ALTER TABLE papers ADD COLUMN IF NOT EXISTS search_vector tsvector`,
		`CREATE INDEX IF NOT EXISTS idx_papers_search ON papers USING GIN(search_vector)`,
		`CREATE OR REPLACE FUNCTION papers_search_trigger() RETURNS trigger AS $$ BEGIN NEW.search_vector := setweight(to_tsvector('english', COALESCE(NEW.title, '')), 'A') || setweight(to_tsvector('english', COALESCE(NEW.abstract, '')), 'B'); RETURN NEW; END $$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS papers_search_update ON papers`,
		`CREATE TRIGGER papers_search_update BEFORE INSERT OR UPDATE ON papers FOR EACH ROW EXECUTE FUNCTION papers_search_trigger()`,
	}
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			return fmt.Errorf("execute schema statement %q: %w", statement, err)
		}
	}
	return nil
}

func rebuildSearchIndex(ctx context.Context, db *gorm.DB) error {
	start := time.Now()
	result := db.WithContext(ctx).Exec(`
		UPDATE papers SET search_vector =
			setweight(to_tsvector('english', COALESCE(title, '')), 'A') ||
			setweight(to_tsvector('english', COALESCE(abstract, '')), 'B')
		WHERE search_vector IS NULL
	`)
	if result.Error != nil {
		return result.Error
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	log.Printf("  Updated %d search vectors in %v", result.RowsAffected, time.Since(start))
	return nil
}
