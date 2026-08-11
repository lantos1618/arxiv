package arxiv_test

import (
	"context"
	"fmt"

	arxiv "github.com/lantos1618/arxiv.gg"
)

func ExampleCache_SyncMetadata() {
	ctx := context.Background()
	cache, err := arxiv.Open("/tmp/arxiv-example")
	if err != nil {
		return
	}
	defer cache.Close()

	_ = cache.SyncMetadata(ctx, &arxiv.SyncOptions{Set: "cs", BatchSize: 1000})
}

func ExampleCache_FetchAndDownload() {
	ctx := context.Background()
	cache, err := arxiv.Open("/tmp/arxiv-example")
	if err != nil {
		return
	}
	defer cache.Close()

	options := &arxiv.DownloadOptions{DownloadPDF: true, DownloadSource: true}
	paper, err := cache.FetchAndDownload(ctx, "2301.00001", options)
	if err == nil {
		fmt.Println(paper.Title)
	}
}
