# Contributing to arXiv Cache Manager

Thank you for your interest in contributing to the arXiv Cache Manager! This document provides guidelines and instructions for contributing.

## Code of Conduct

Please be respectful and constructive in all interactions. We aim to maintain a welcoming environment for all contributors.

## Getting Started

### Prerequisites

- Go 1.18 or later
- SQLite with FTS5 support
- `pdftotext` (from poppler-utils) for PDF text extraction
- Python 3.8+ (optional, for embedding generation)

### Setting Up Development Environment

1. Clone the repository:
   ```bash
   git clone https://github.com/tmc/arxiv.git
   cd arxiv
   ```

2. Install dependencies:
   ```bash
   go mod download
   ```

3. Run tests to verify setup:
   ```bash
   make test
   ```

## Development Workflow

### Making Changes

1. Create a new branch for your feature or fix:
   ```bash
   git checkout -b feature/your-feature-name
   ```

2. Make your changes following the coding standards below.

3. Run tests and checks:
   ```bash
   make check
   ```

4. Commit your changes with a clear, descriptive message:
   ```bash
   git commit -m "Add feature: description of changes"
   ```

### Coding Standards

#### Go Code

- Follow standard Go conventions and idioms
- Use `gofmt` to format code (run `make fmt`)
- Run `go vet` before committing (run `make vet`)
- Write tests for new functionality
- Keep functions focused and small
- Document exported types, functions, and methods
- Use meaningful variable and function names
- Handle errors explicitly - don't ignore them

#### File Organization

```
arxiv/
├── *.go              # Main package files
├── *_test.go         # Test files
├── internal/         # Internal packages
│   ├── cache/        # Cache management
│   ├── citations/    # Citation handling
│   ├── data/         # Data operations
│   ├── export/       # Export formats
│   └── search/       # Search functionality
├── cmd/arxiv/        # CLI application
├── tools/            # Utility scripts
└── docs/             # Documentation
```

#### Testing

- Write table-driven tests when appropriate
- Use subtests for related test cases
- Test both success and error paths
- Use `t.TempDir()` for temporary test directories
- Mock external dependencies when possible

Example test structure:
```go
func TestFunction(t *testing.T) {
    testCases := []struct {
        name     string
        input    string
        expected string
    }{
        {"valid input", "foo", "bar"},
        {"empty input", "", ""},
    }
    
    for _, tc := range testCases {
        t.Run(tc.name, func(t *testing.T) {
            result := Function(tc.input)
            if result != tc.expected {
                t.Errorf("got %s, want %s", result, tc.expected)
            }
        })
    }
}
```

### Pull Request Process

1. Ensure all tests pass: `make test`
2. Run code checks: `make check`
3. Update documentation if needed
4. Create a pull request with:
   - Clear title describing the change
   - Description of what and why (not just how)
   - Reference to related issues (if any)

### Commit Messages

Write clear, concise commit messages:

```
Add semantic search with embedding support

- Implement cosine similarity for vector comparison
- Add StoreEmbedding and GetEmbedding methods
- Support hybrid search combining FTS5 and semantic

Fixes #123
```

## Areas for Contribution

### Good First Issues

- Adding more tests
- Improving documentation
- Fixing typos
- Adding examples

### Intermediate

- Implementing new export formats
- Improving search algorithms
- Adding CLI features
- Performance optimizations

### Advanced

- Native Go embedding generation
- Distributed caching support
- Advanced citation analysis
- Web interface improvements

## Reporting Issues

When reporting issues, please include:

1. Clear description of the problem
2. Steps to reproduce
3. Expected vs actual behavior
4. Go version and OS
5. Relevant error messages or logs

## Questions?

If you have questions about contributing, please open an issue with the "question" label.

## License

By contributing, you agree that your contributions will be licensed under the same license as the project (MIT).
