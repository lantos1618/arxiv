# arxiv.gg Admin

The admin UI exposes operational database metrics, users and signed-in paper views, audit events, and feedback moderation.

## Routes

- `/admin` — database, catalog, downloads, Qwen/embedding coverage, jobs, and recent activity
- `/admin/users` — accounts, sessions, and signed-in paper-view aggregates
- `/admin/audit` — admin page-view and moderation audit events
- `/admin/feedback` — visible/hidden feedback and moderation controls

Metrics refresh in the background and may lag active workers. User analytics cover accounts, sessions, and signed-in paper views only; anonymous visitors, search queries, billing, and payments are not represented.

## Access

In production, access is granted by either:

- a signed-in Google account whose email appears in `ADMIN_EMAILS`; or
- `X-Admin-Token: <ADMIN_TOKEN>` / `Authorization: Bearer <ADMIN_TOKEN>`.

Reusable admin secrets are not accepted in query strings. If neither `ADMIN_TOKEN` nor `ADMIN_EMAILS` is configured, admin endpoints are disabled. `serve -local` bypasses these checks but binds only to loopback.

Cookie-authenticated moderation requires same-origin requests. Keep the admin UI behind HTTPS, restrict ingress, rotate exposed tokens, and do not share account API keys as admin credentials.

## Feedback

Signed-in users can submit, vote on, and delete their own suggestions. Administrators can hide or restore posts; hidden posts remain stored. “Post anonymously” hides the public display name but does not remove the user association from operator views or the database.

The `$100` copy in the public widget is a conditional, discretionary offer, not an automatic bounty. A qualifying idea must be selected, actually shipped, accepted under the offer, and submitted by someone who can be contacted and paid legally. No award is guaranteed for every post, vote leader, overlapping idea, partial implementation, or independently planned feature.
