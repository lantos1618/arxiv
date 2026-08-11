# SEO Indexing Report - 2026-05-16

> **ARCHIVED SEARCH-INDEXING SNAPSHOT.** This report records crawler observations and actions from 2026-05-16. Index status, sitemap counts, search-engine behavior, and expected outcomes are time-sensitive and must not be treated as current. Use the live sitemap endpoints and fresh webmaster-console data for present decisions.

## Findings

- Google Search Console is correct that live `robots.txt` contains an unknown directive:
  `Content-Signal: search=yes,ai-train=no`.
- The origin app does not emit that line. Cloudflare prepends it through its managed
  `robots.txt` / AI crawler setting.
- The old `/sitemap.xml` only exposed static pages, category pages, and 50 recent papers
  per category, which produced about 5,848 discovered URLs.
- The database currently contains 644,168 distinct paper IDs. There are also 130 duplicate
  historical rows by paper ID, so sitemap generation now deduplicates by ID without changing
  the database.

## Changes Made

- `/sitemap.xml` now serves a sitemap index.
- `/sitemap-static.xml` serves the home page, categories page, and category pages.
- `/sitemaps/papers-N.xml` serves distinct paper pages in chunks of up to 50,000 URLs.
- Sitemap and robots handlers now allow `HEAD` requests.
- Sitemaps are returned with `Cache-Control: public, max-age=3600`.
- Paper, author, and category pages now emit canonical URLs and page-specific meta
  descriptions.
- Paper pages emit `ScholarlyArticle` JSON-LD, author pages emit `ProfilePage` JSON-LD,
  and category pages emit `CollectionPage` / `ItemList` JSON-LD.
- `/34af0c26368622541e3ca8aa555c3ad7.txt` serves the IndexNow key file.
- `tools/submit_indexnow.py` can submit changed URLs, a URL file, or sitemap URLs to
  IndexNow in batches.

## Expected Live Sitemap Shape

- 1 static sitemap.
- 13 paper sitemap chunks.
- 50,000 paper URLs in chunks 1-12.
- 44,168 paper URLs in chunk 13.
- 644,168 distinct paper URLs total.

## Cloudflare Action Still Needed

Cloudflare documents that managed `robots.txt` prepends its own block before the origin
`robots.txt`: https://developers.cloudflare.com/bots/additional-configurations/managed-robots-txt/.
To remove the Google Search Console warning, disable the Content Signals / managed robots
display in Cloudflare:

1. Open Cloudflare dashboard for `arxiv.gg`.
2. Go to the zone overview or Security Settings.
3. Find Control AI Crawlers / Instruct AI bot traffic with robots.txt.
4. Uncheck Display Content Signals Policy, or disable managed `robots.txt` entirely.
5. Purge Cloudflare cache for `https://arxiv.gg/robots.txt`.

API note: the available Cloudflare token verified successfully and can list the `arxiv.gg`
zone, but Cloudflare returned `9109 Unauthorized to access requested resource` for zone
settings reads. The managed robots toggle still needs to be completed in the dashboard
unless a token with the required settings surface is provided.

After this, live `robots.txt` should only show the origin app output:

```txt
User-agent: *
Disallow:

Sitemap: https://arxiv.gg/sitemap.xml
```

## IndexNow Submission

IndexNow should be used for changed or newly exposed URLs rather than as a daily full-corpus
ping. The current key is public and must match the key file served by the app.

Examples:

```sh
python3 tools/submit_indexnow.py --url https://arxiv.gg/sitemap.xml
python3 tools/submit_indexnow.py --sitemap https://arxiv.gg/sitemap.xml --limit 1000
python3 tools/submit_indexnow.py --file changed-urls.txt
```

The script submits JSON to `https://api.indexnow.org/indexnow` with `host`, `key`,
`keyLocation`, and `urlList`, in batches of up to 10,000 URLs.

## Follow-Up for "Crawled - Currently Not Indexed"

Search Console examples showed crawlable `200` pages in the "Crawled - currently not
indexed" bucket. The follow-up technical fixes are:

- Add self-referencing canonical URLs to paper and author pages.
- Add page-specific descriptions for author pages instead of the generic site description.
- Add `ScholarlyArticle` JSON-LD to paper pages.
- Add `ProfilePage` / `Person` / `ItemList` JSON-LD to author pages.
- Redirect author URLs with trailing slashes to the canonical author URL.
- Redirect versioned arXiv abstract URLs, such as `/abs/2105.14275v1`, to the base paper URL.
- Change internal navigational links from redirecting `/paper/{id}` URLs to canonical `/abs/{id}` URLs.

These changes do not guarantee indexing, but they remove avoidable ambiguity and give Google
clearer canonical, entity, and page-quality signals for pages it already crawled.
