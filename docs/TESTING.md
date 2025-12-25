# Testing

## Test Files

The project includes comprehensive tests for core functionality:

### `semantic_test.go`
- `TestCosineSimilarity` - Tests cosine similarity calculation (same vectors, orthogonal vectors, edge cases)
- `TestStoreEmbedding` - Tests embedding storage and retrieval
- `TestSearchSemantic` - Tests semantic search with empty and populated databases

### `embeddings_test.go`
- `TestEmbeddingSerialization` - Tests that embedding serialization/deserialization preserves values
- `TestEmbeddingSerializationEmpty` - Tests error handling for empty embeddings
- `TestEmbeddingDeserializationInvalid` - Tests error handling for invalid embedding data
- `TestListPapersWithoutEmbeddings` - Tests listing papers that need embeddings
- `TestCountEmbeddings` - Tests counting embeddings in the database

### `cache_test.go`
- `TestCacheOpen` - Tests cache initialization
- `TestCacheStats` - Tests statistics retrieval
- `TestCachePaperOperations` - Tests paper creation, retrieval, and existence checks

### `search_test.go`
- `TestSearchEmpty` - Tests search on empty cache
- `TestSearchWithPapers` - Tests keyword search with papers and category filtering
- `TestSearchByAuthor` - Tests author-based search

## Running Tests

```bash
# Run all tests
go test ./...

# Run with verbose output
go test -v ./...

# Run specific test
go test -v -run TestStoreEmbedding

# Run tests with coverage
go test -cover ./...

# Generate coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

## Test Coverage

Current test coverage includes:
- ✅ Embedding storage and retrieval
- ✅ Semantic search functionality
- ✅ Cosine similarity calculation
- ✅ Cache operations
- ✅ Paper CRUD operations
- ✅ Keyword search (with FTS5 fallback handling)
- ✅ Author search

## Notes

- Tests use temporary directories created with `t.TempDir()`
- FTS5 may not be available in test environments - tests handle this gracefully
- All tests are isolated and can run in parallel
- Tests use the same `package arxiv` as the main code

## Adding New Tests

When adding new functionality, add corresponding tests:

1. Create a `*_test.go` file in the same package
2. Use `t.TempDir()` for test data
3. Test both success and error cases
4. Use descriptive test names: `TestFunctionName_Scenario`

Example:
```go
func TestNewFeature(t *testing.T) {
    cacheDir := t.TempDir()
    cache, err := Open(cacheDir)
    if err != nil {
        t.Fatalf("Failed to open cache: %v", err)
    }
    defer cache.Close()
    
    // Test implementation
}
```

