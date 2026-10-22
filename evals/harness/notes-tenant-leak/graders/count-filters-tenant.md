---
type: regex
target: { source: file, path: nexus-platform/internal/notes/repository.go }
pattern: 'func \(r \*Repository\) CountByRecord[\s\S]*?tenant_id\s*=\s*\?[\s\S]*?func \(r \*Repository\) GetForRecord'
---
