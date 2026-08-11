package main

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/lantos1618/arxiv.gg"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openTestDB(t *testing.T, name string, models ...any) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), name+".db")), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestMigrateSyncStateCopiesRows(t *testing.T) {
	src := openTestDB(t, "src", &arxiv.SyncState{})
	dst := openTestDB(t, "dst", &arxiv.SyncState{})
	if err := src.Create(&arxiv.SyncState{Key: "cursor", Value: "next"}).Error; err != nil {
		t.Fatal(err)
	}

	if err := migrateSyncState(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	var state arxiv.SyncState
	if err := dst.First(&state, "key = ?", "cursor").Error; err != nil {
		t.Fatal(err)
	}
	if state.Value != "next" {
		t.Fatalf("value = %q, want next", state.Value)
	}
	if err := src.Model(&arxiv.SyncState{}).Where("key = ?", "cursor").Update("value", "updated").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateSyncState(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	if err := dst.First(&state, "key = ?", "cursor").Error; err != nil {
		t.Fatal(err)
	}
	if state.Value != "updated" {
		t.Fatalf("value after resume = %q, want updated", state.Value)
	}
}

func TestCurrentModelsCoversApplicationSchema(t *testing.T) {
	models := currentModels()
	if len(models) != 22 {
		t.Fatalf("model count = %d, want 22", len(models))
	}
	names := make(map[string]bool, len(models))
	for _, model := range models {
		names[model.name] = true
	}
	for _, name := range []string{"download queue", "paper chunks", "users", "user API keys", "feedback posts", "admin audit log"} {
		if !names[name] {
			t.Errorf("missing model migration %q", name)
		}
	}
}

func TestMigrateRowsHonorsCancellation(t *testing.T) {
	src := openTestDB(t, "src", &arxiv.Paper{})
	dst := openTestDB(t, "dst", &arxiv.Paper{})
	if err := src.Create(&arxiv.Paper{ID: "2601.00001"}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := migrateRows[arxiv.Paper](ctx, src, dst, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

func TestParseMigrationConfig(t *testing.T) {
	config, err := parseMigrationConfig([]string{"-sqlite", "/tmp/source.db", "-batch", "25"}, func(key string) string {
		if key == "DATABASE_URL" {
			return "postgres://example"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.batchSize != 25 || config.postgresURL != "postgres://example" {
		t.Fatalf("config = %+v", config)
	}
	if _, err := parseMigrationConfig([]string{"-batch", "0", "-postgres", "postgres://example"}, func(string) string { return "" }); err == nil {
		t.Fatal("expected invalid batch error")
	}
}

func TestPostgresVectorConvertsSQLiteFloat32Blob(t *testing.T) {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint32(data[:4], math.Float32bits(1.5))
	binary.LittleEndian.PutUint32(data[4:], math.Float32bits(-2.25))
	value, err := postgresVector(data)
	if err != nil {
		t.Fatal(err)
	}
	if value != "[1.5,-2.25]" {
		t.Fatalf("vector = %q", value)
	}
}

func TestMigrateVectorColumnCopiesBlob(t *testing.T) {
	src := openTestDB(t, "src", &arxiv.Embedding{})
	dst := openTestDB(t, "dst", &arxiv.Embedding{})
	for _, db := range []*gorm.DB{src, dst} {
		if err := db.Exec(`ALTER TABLE embeddings ADD COLUMN vector BLOB`).Error; err != nil {
			t.Fatal(err)
		}
	}
	vector := []byte{1, 2, 3, 4}
	if err := src.Exec(`INSERT INTO embeddings (paper_id, model, created, vector) VALUES (?, ?, CURRENT_TIMESTAMP, ?)`, "paper", "model", vector).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateEmbeddings(context.Background(), src, dst, 10); err != nil {
		t.Fatal(err)
	}
	var got []byte
	sqlDB, err := dst.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.QueryRow(`SELECT vector FROM embeddings WHERE paper_id = ?`, "paper").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if string(got) != string(vector) {
		t.Fatalf("vector = %v, want %v", got, vector)
	}
}

func TestMigrateSyncStatePropagatesInsertError(t *testing.T) {
	src := openTestDB(t, "src", &arxiv.SyncState{})
	dst := openTestDB(t, "dst", &arxiv.SyncState{})
	if err := src.Create(&arxiv.SyncState{Key: "cursor", Value: "next"}).Error; err != nil {
		t.Fatal(err)
	}
	forced := errors.New("forced insert failure")
	if err := dst.Callback().Create().Before("gorm:create").Register("test:fail_create", func(tx *gorm.DB) {
		tx.AddError(forced)
	}); err != nil {
		t.Fatal(err)
	}

	err := migrateSyncState(context.Background(), src, dst)
	if err == nil || !strings.Contains(err.Error(), forced.Error()) {
		t.Fatalf("error = %v, want forced insert failure", err)
	}
}

func TestMigratePapersPropagatesFallbackInsertError(t *testing.T) {
	src := openTestDB(t, "src", &arxiv.Paper{})
	dst := openTestDB(t, "dst", &arxiv.Paper{})
	if err := src.Create(&arxiv.Paper{ID: "2601.00001", Title: "test"}).Error; err != nil {
		t.Fatal(err)
	}
	forced := errors.New("forced paper insert failure")
	if err := dst.Callback().Create().Before("gorm:create").Register("test:fail_create", func(tx *gorm.DB) {
		tx.AddError(forced)
	}); err != nil {
		t.Fatal(err)
	}

	err := migratePapers(context.Background(), src, dst, 10)
	if err == nil || !strings.Contains(err.Error(), forced.Error()) {
		t.Fatalf("error = %v, want forced paper insert failure", err)
	}
}
