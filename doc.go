// Package arxiv provides a local arXiv metadata cache, search index, download
// manager, citation graph, and optional semantic-search storage.
//
// Open creates the cache directory and uses SQLite by default. When DATABASE_URL
// is set, Open uses PostgreSQL instead. Metadata synchronization uses arXiv's
// OAI-PMH endpoint and stores resumable synchronization state. PDFs and TeX
// sources are downloaded only when explicitly requested through DownloadPaper
// or FetchAndDownload; a metadata sync does not create a complete offline mirror.
//
// A minimal metadata synchronization looks like:
//
//	cache, err := arxiv.Open("/path/to/cache")
//	if err != nil {
//		return err
//	}
//	defer cache.Close()
//
//	opts := &arxiv.SyncOptions{BatchSize: 1000}
//	if err := cache.SyncMetadata(ctx, opts); err != nil {
//		return err
//	}
//
// Fetching metadata and both available artifact types for one paper looks like:
//
//	opts := &arxiv.DownloadOptions{DownloadPDF: true, DownloadSource: true}
//	paper, err := cache.FetchAndDownload(ctx, "2301.00001", opts)
//	if err != nil {
//		return err
//	}
//	fmt.Println(paper.Title)
package arxiv
