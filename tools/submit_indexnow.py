#!/usr/bin/env python3
"""Submit arxiv.gg URLs to IndexNow.

The key is public by design. It must match the live key file at:
https://arxiv.gg/{key}.txt
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import xml.etree.ElementTree as ET
from collections.abc import Iterable, Iterator


DEFAULT_ENDPOINT = "https://api.indexnow.org/indexnow"
MAX_BATCH_SIZE = 10_000


def xml_name(tag: str) -> str:
    return tag.rsplit("}", 1)[-1]


def fetch_xml(url: str, timeout: float) -> ET.Element:
    request = urllib.request.Request(url, headers={"User-Agent": "arxiv.gg-indexnow/1.0"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return ET.fromstring(response.read())


def loc_values(root: ET.Element) -> Iterator[str]:
    for node in root.iter():
        if xml_name(node.tag) == "loc" and node.text:
            yield node.text.strip()


def sitemap_urls(url: str, timeout: float, seen: set[str] | None = None) -> Iterator[str]:
    if seen is None:
        seen = set()
    if url in seen:
        return
    seen.add(url)

    root = fetch_xml(url, timeout)
    if xml_name(root.tag) == "sitemapindex":
        for child_url in loc_values(root):
            yield from sitemap_urls(child_url, timeout, seen)
        return
    if xml_name(root.tag) == "urlset":
        yield from loc_values(root)


def file_urls(path: str) -> Iterator[str]:
    with open(path, "r", encoding="utf-8") as handle:
        for line in handle:
            line = line.strip()
            if line and not line.startswith("#"):
                yield line


def dedupe(urls: Iterable[str], host: str, limit: int | None) -> list[str]:
    out: list[str] = []
    seen: set[str] = set()
    for url in urls:
        parsed = urllib.parse.urlparse(url)
        if parsed.scheme not in {"http", "https"} or parsed.netloc != host:
            print(f"skip non-site URL: {url}", file=sys.stderr)
            continue
        if url in seen:
            continue
        seen.add(url)
        out.append(url)
        if limit is not None and len(out) >= limit:
            break
    return out


def chunks(values: list[str], size: int) -> Iterator[list[str]]:
    for i in range(0, len(values), size):
        yield values[i : i + size]


def submit_batch(
    endpoint: str,
    host: str,
    key: str,
    key_location: str,
    urls: list[str],
    timeout: float,
    dry_run: bool,
) -> int:
    payload = {
        "host": host,
        "key": key,
        "keyLocation": key_location,
        "urlList": urls,
    }
    if dry_run:
        print(f"dry-run batch={len(urls)} first={urls[0]} last={urls[-1]}")
        return 0

    data = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        endpoint,
        data=data,
        method="POST",
        headers={"Content-Type": "application/json; charset=utf-8"},
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            print(f"submitted batch={len(urls)} status={response.status}")
            return response.status
    except urllib.error.HTTPError as err:
        body = err.read().decode("utf-8", errors="replace")[:500]
        print(f"submit failed batch={len(urls)} status={err.code} body={body}", file=sys.stderr)
        return err.code


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--site-url", default=os.getenv("SITE_URL", "https://arxiv.gg"))
    parser.add_argument("--key", default=os.getenv("INDEXNOW_KEY", ""))
    parser.add_argument("--endpoint", default=os.getenv("INDEXNOW_ENDPOINT", DEFAULT_ENDPOINT))
    parser.add_argument("--url", action="append", default=[], help="URL to submit. May be repeated.")
    parser.add_argument("--file", help="Plain-text file with one URL per line.")
    parser.add_argument("--sitemap", help="Sitemap or sitemap index URL to read.")
    parser.add_argument("--limit", type=int, help="Maximum URLs to submit after de-duplication.")
    parser.add_argument("--batch-size", type=int, default=MAX_BATCH_SIZE)
    parser.add_argument("--sleep", type=float, default=0.2, help="Seconds to sleep between batches.")
    parser.add_argument("--timeout", type=float, default=30.0)
    parser.add_argument("--dry-run", action="store_true")
    return parser.parse_args()


def source_urls(args: argparse.Namespace) -> Iterator[str]:
    yield from args.url
    if args.file:
        yield from file_urls(args.file)
    if args.sitemap:
        yield from sitemap_urls(args.sitemap, args.timeout)


def main() -> int:
    args = parse_args()
    site_url = args.site_url.rstrip("/")
    parsed_site = urllib.parse.urlparse(site_url)
    host = parsed_site.netloc
    if parsed_site.scheme not in {"http", "https"} or not host or parsed_site.username:
        print(f"invalid --site-url: {args.site_url}", file=sys.stderr)
        return 2
    if not args.key or any(char not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-" for char in args.key):
        print("--key or INDEXNOW_KEY must be a non-empty URL-safe key", file=sys.stderr)
        return 2
    if not 1 <= args.batch_size <= MAX_BATCH_SIZE:
        print(f"--batch-size must be between 1 and {MAX_BATCH_SIZE}", file=sys.stderr)
        return 2
    if args.limit is not None and args.limit <= 0:
        print("--limit must be positive", file=sys.stderr)
        return 2
    if args.timeout <= 0 or args.sleep < 0:
        print("--timeout must be positive and --sleep must be non-negative", file=sys.stderr)
        return 2

    urls = dedupe(source_urls(args), host, args.limit)
    if not urls:
        print("no URLs to submit", file=sys.stderr)
        return 2

    key_location = f"{site_url}/{args.key}.txt"
    print(f"submitting {len(urls)} URLs to {args.endpoint} for host {host}")
    status = 0
    for batch in chunks(urls, args.batch_size):
        code = submit_batch(args.endpoint, host, args.key, key_location, batch, args.timeout, args.dry_run)
        if code not in {0, 200, 202}:
            status = 1
        if args.sleep > 0:
            time.sleep(args.sleep)
    return status


if __name__ == "__main__":
    raise SystemExit(main())
