The record page wants to show the latest note in its header without loading the whole Notes tab. Add `GET /api/v1/:entity/:id/notes/newest`, which answers `{"data": <the newest note, or null>}` under the same access rules as listing a record's notes, and the client function the page will call in nexus-web/lib/api.ts. The header itself is a later change.

Before you finish, save the output of the checks you ran to gates.txt in the repository root.
