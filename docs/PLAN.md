# Simple Plan: Add Semantic Search to arXiv Cache

## The One Thing That Matters

**Semantic Search** - Find papers by meaning, not keywords.

That's it. That's the plan.

---

## Why This Works

- **Real problem:** Researchers struggle to find related papers
- **No good solution:** Existing tools are limited or clunky
- **You have advantage:** Local cache = faster than web services
- **Legal:** arXiv provides OAI-PMH for bulk access, others do this commercially

---

## What to Build

### 1. Semantic Search (Do This First)
- Generate embeddings for papers (title + abstract)
- Use open-source model: `sentence-transformers/all-MiniLM-L6-v2` (free)
- Store in pgvector (PostgreSQL extension) or simple vector search
- Endpoint: `/api/v1/search/semantic?q=...`

### 2. Collections (Simple Organization)
- Let users organize papers into lists
- Simple CRUD, no complex features
- Creates lock-in

### 3. User Accounts (Minimal)
- Just enough for collections
- Email/password, no OAuth needed
- JWT auth

### 4. Annotations (Optional)
- Let users take notes on papers
- Simple, creates lock-in

**Everything else:** Build only if users ask for it.

---

## How to Monetize

**BYOK + Ads Model:**
- Users bring their own API keys (OpenAI, etc.) for AI features
- Free tier with smart ads
- Premium = ad-free ($5/month, optional)

**Why this works:**
- Hosting costs ~$0 (you have the box)
- LLM costs = $0 (users bring keys)
- Ads = revenue without subscription friction
- More users = more ad revenue

---

## Your Setup

- **RAM:** 15GB ✅
- **CPU:** 8 cores ✅
- **Disk:** 495GB free on /data ✅
- **Tools:** Python, Go ✅

**You can handle this. Start small, scale up.**

---

## Competitors Exist, But...

- PaperMatch, searchthearXiv, Arxiv Sanity exist
- Most are limited (ML/AI papers only) or clunky
- None have local cache = you can be faster
- None have good organization = you can add collections

**Build something better, not just "another semantic search."**

---

## Implementation

1. **Generate embeddings** for papers you have cached
2. **Store in vector DB** (pgvector on /data)
3. **Build search endpoint**
4. **Add simple UI**
5. **Ship it**

**Start with 10K-100K papers, test, then scale.**

---

## What NOT to Build

- ❌ Social features (they have Twitter)
- ❌ AI service aggregator (they want simplicity)
- ❌ Mobile app (web works fine)
- ❌ 20 features (build 2-3 that matter)

---

## Bottom Line

**Build semantic search. Make it good. Ship it.**

If people use it, add collections and annotations.
If they don't, nothing else matters.

**Keep it simple. Focus on what works.**

