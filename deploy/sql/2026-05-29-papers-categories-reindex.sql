-- Rebuild the low-cardinality category btree without posting-list
-- deduplication. During OAI metadata expansion, the old index hit btree
-- posting-list split errors on large category runs.
--
-- Run outside a transaction because REINDEX CONCURRENTLY requires it.

SELECT 'ALTER INDEX idx_papers_categories SET (deduplicate_items = off)'
WHERE to_regclass('public.idx_papers_categories') IS NOT NULL
\gexec

SELECT 'REINDEX INDEX CONCURRENTLY idx_papers_categories'
WHERE to_regclass('public.idx_papers_categories') IS NOT NULL
\gexec
