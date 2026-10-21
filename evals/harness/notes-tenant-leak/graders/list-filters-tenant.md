---
type: regex
target: { source: file, path: nexus-platform/internal/notes/repository.go }
pattern: 'func \(r \*Repository\) ListByRecord[\s\S]*?tenant_id\s*=\s*\?[\s\S]*?func \(r \*Repository\) CountByRecord'
---
