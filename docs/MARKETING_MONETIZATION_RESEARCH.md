# arxiv.gg Marketing And Monetization Research

> **ARCHIVED RESEARCH SNAPSHOT — NO CURRENT OFFER OR ROADMAP AUTHORITY.** This document preserves exploratory positioning and pricing research from its original review period. It does not announce plans, prices, paid tiers, entitlement, revenue, or product commitments. Current capabilities and the conditional feedback-offer terms are documented in `README.md`.

Status: research draft / maybe plan  
Date: 2026-05-16

## Short Version

Do not charge for access to arXiv papers. Keep public paper, author, category, search, and citation pages open for SEO and trust.

Charge for workflow: alerts, saved research memory, summaries, semantic monitoring, digests, team reading lists, exports, and API access.

The strongest product wedge is:

> arxiv.gg is a research radar for people who cannot keep up with arXiv.

## Current Asset

arxiv.gg already has:

- 644,298 indexed papers.
- 644,168 embeddings.
- 21.2% arXiv.org corpus coverage as of 2026-05-16.
- Searchable paper pages.
- Author and category pages.
- Semantic search.
- Citation graph features.
- Full sitemap shards and IndexNow.
- GitHub/project credibility.

That means the free product should stay open and crawlable. The paid product should sit on top of the graph and search index.

## Compliance Notes

Before monetizing, add or confirm:

- Clear footer: `Not affiliated with arXiv.org`.
- arXiv data acknowledgement: `Thank you to arXiv for use of its open access interoperability.`
- Link users to arxiv.org for PDFs/source unless a paper license clearly permits redistribution.
- Avoid branding that implies arXiv endorsement.

Relevant arXiv docs:

- https://info.arxiv.org/help/api/index.html
- https://info.arxiv.org/help/api/tou.html
- https://info.arxiv.org/help/license/index.html

## Target Users

Best early buyers:

- ML engineers tracking new papers.
- PhD students trying to keep up with a subfield.
- AI lab researchers watching specific authors, labs, and topics.
- Startup teams monitoring technical moats.
- Quant / physics / math people with narrow daily feeds.

Weak buyers:

- Casual search users.
- People who only land from Google for one paper.
- Students looking for a free PDF.

## Positioning

Possible headline:

> Keep up with arXiv without reading arXiv all day.

Supporting copy:

> Follow authors, topics, and semantic research questions. Get daily paper radar, citation alerts, and concise briefs when something actually matters.

Avoid positioning:

- "Better arXiv"
- "arXiv Pro"
- "Official arXiv"
- Anything that sounds affiliated or endorsed.

## Free Product

Keep these free:

- Search.
- Paper pages.
- Author pages.
- Category pages.
- Citation graph preview.
- Basic semantic search.
- Public API stats.
- 20 saved papers.
- 3 follows across authors/categories/topics.
- Weekly digest.

Reason: free pages build traffic, backlinks, search trust, and the top of funnel.

## Paid Product

### Pro: $12-15/month

For individual researchers and builders.

- Unlimited saved papers.
- Unlimited author/category/topic follows.
- Daily personalized paper radar.
- Semantic alerts, for example: "new papers like efficient long-context inference."
- Paper briefs:
  - TL;DR
  - contribution
  - methods
  - limitations
  - why it matters
  - related papers
- Citation alerts: "new paper cited this."
- Export bundles: BibTeX, RIS, CSV, Zotero-friendly.
- Private notes and reading queue.

### Power: $29/month

For heavy users.

- Everything in Pro.
- Literature briefs across a topic.
- Compare selected papers.
- Research gap finder.
- "What changed this week/month?" reports.
- More semantic alerts.
- API key with reasonable limits.

### Teams / Labs: $99-299/month

For small labs and companies.

- Shared reading lists.
- Shared annotations.
- Team digests.
- Slack/email alerts.
- Watchlists for authors, labs, competitors, keywords, and semantic topics.
- Admin billing.

### Enterprise: Custom

Only after pull from users.

- SSO.
- Higher API limits.
- Custom data sources.
- Private deployment.
- Procurement/security docs.

## Best First Paid Feature

Build semantic alerts first.

Example:

> Alert me when a new paper is about inference optimization for long-context LLMs, even if it does not mention those exact words.

Why this is the best wedge:

- It is clearly differentiated from raw arXiv search.
- It saves daily attention.
- It uses existing embeddings.
- It creates recurring usage.
- It naturally supports email capture.

## Feature Ideas

High priority:

- Account system.
- Saved papers.
- Follow author/category/topic.
- Semantic alert queries.
- Daily/weekly email digest.
- Simple Stripe subscription.
- "Why this matters" paper brief.

Medium priority:

- Citation alerting.
- Team reading lists.
- Zotero integration.
- Slack digest.
- Browser extension.
- API keys.

Later:

- Full literature review agent.
- Figure/table extraction.
- Collaborative annotation.
- Institutional plans.
- Personal knowledge graph.

## Landing Page Angle

The first screen should not be a generic marketing page. It should be a working search/radar interface with upgrade hooks.

Suggested page modules:

- Search bar: "Search or follow a topic..."
- Coverage badge: "644k papers indexed, 644k embedded, 21.2% arXiv coverage."
- Example alert chips:
  - long-context inference
  - LLM agents
  - diffusion policy
  - quantum error correction
  - mechanistic interpretability
- CTA:
  - Free: "Search papers"
  - Paid: "Create research radar"

## Acquisition

SEO:

- Keep every paper/category/author page public.
- Improve titles/meta descriptions.
- Add paper briefs to high-impression pages.
- Keep sitemap and IndexNow current.

Community:

- Show HN: "I built a semantic arXiv radar with 644k embedded papers."
- Reddit posts in r/MachineLearning, r/Physics, r/math, r/LocalLLaMA where allowed.
- Deep links to useful paper/category pages, not just homepage.
- Author outreach: "Your paper page has citation graph + related papers."

Content:

- Weekly "What changed in AI papers this week?"
- Category reports:
  - "Top new cs.LG papers this week"
  - "New long-context papers"
  - "New papers citing Attention Is All You Need"

## Metrics To Watch

Top of funnel:

- Indexed pages.
- Impressions.
- CTR.
- Search visits.
- Paper page exits.
- Return visits.

Activation:

- Searches per visitor.
- Saved papers.
- Follows created.
- Digest signups.
- Semantic alert creation.

Paid:

- Free-to-Pro conversion.
- Digest open rate.
- Alert click-through rate.
- Churn after first month.
- Cost per AI brief.

## Pricing Research Anchors

Comparable research tools already charge for AI/search workflow:

- Consensus has free and paid research-search plans, with Pro around the low double-digit monthly range.
- Elicit has paid tiers for deeper research workflows, reports, exports, API access, and teams.

This supports a starting price around $12-15/month for Pro and $29/month for Power, as long as the product is about recurring workflow rather than raw access.

Sources:

- https://help.consensus.app/en/articles/10087865-subscription-plans
- https://elicit.com/pricing

## Risks

- arXiv branding/commercial compliance.
- Serving or summarizing content in ways that conflict with individual paper licenses.
- AI summaries creating trust problems.
- Too much generic "chat with paper" competition.
- Search traffic is useful but may not convert unless there is a recurring workflow.

## Recommendation

Build a narrow paid beta:

1. Accounts.
2. Saved papers.
3. Follow authors/categories.
4. Semantic alerts.
5. Weekly email digest.
6. Stripe Pro at $12/month or $120/year.

Do not build team features until individual users are saving/following/returning.

## Open Questions

- Which segment converts first: ML engineers, PhD students, or startup teams?
- Should Pro start with email digests only, or include AI briefs immediately?
- How much can be summarized safely from metadata/abstracts alone?
- Should the product brand become less arXiv-like if paid plans become meaningful?
- What is the fastest path to daily active use without harming SEO?
