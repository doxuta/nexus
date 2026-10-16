package main

// Apply-then-probe — TRỤ 4: stop measuring "the assistant said something on-topic" and start
// measuring "the thing it proposed, when applied, is an app you can actually put data in".
//
// Everything the harness scored before this file is SHAPE: a draft came back, it had the right
// kinds, it named the right entity. That is a real signal (every propose_* validates with the same
// validator the apply path uses) but it stops one step short of the only question a customer cares
// about. A proposal can be perfectly shaped and still yield a table nobody can write a row into.
//
// So: apply the backbone of the draft through the SAME admin APIs the Studio's Apply button calls
// — no private "eval-only" write path, because a write path that only the harness uses would be
// measuring itself — then ask the running server three questions in the customer's order:
//
//	1. is the entity in /metadata/entities?      (metadata registered)
//	2. does POST /:entity accept a row?          (the physical table exists and takes data)
//	3. does GET /:entity/:id give it back?       (it is readable, not just accepted)
//
// Only `entity` and `field` drafts are applied. That is the backbone — the physical table and its
// columns — and it is deliberately NOT the whole draft: layouts/apps/roles are cosmetics and
// permissions on top of a table that either exists or does not. A case whose draft carries other
// kinds still scores its shape metrics and reports the kinds it skipped, so nothing is silently
// counted as covered.
//
// ponytail: no cleanup framework — the probe records what it created and deletes it in reverse on
// the way out, reporting anything it could not remove. An eval that quietly litters the lab tenant
// with half-built tables poisons every later run, including its own re-runs.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// probeSpec is the golden file's per-case description of what "usable" means for THIS case.
type probeSpec struct {
	// Entity is the entity the probe writes a row into. Empty → the first applied entity.
	Entity string `json:"entity,omitempty"`
	// Row is the record body POSTed. Keep it minimal — the point is that the table accepts data,
	// not that the model guessed a schema this harness could fill.
	Row map[string]any `json:"row,omitempty"`
}

// applyResult is what the probe reports back into the case's detail line.
type applyResult struct {
	Created  []string // entity names created, newest last (deleted in reverse)
	Skipped  []string // draft kinds NOT applied — reported so "probed" never reads as "all of it"
	Failures []string
}

// draftFull is the draft item with the payloads the apply path needs. The scoring path decodes only
// Kind; this one needs the bodies, and keeps them as raw JSON so an unknown future key is forwarded
// rather than silently dropped by a partial struct (the exact bug that made ExpectEntity lie once).
type draftFull struct {
	Kind   string `json:"kind"`
	Entity *struct {
		Name   string            `json:"name"`
		Label  string            `json:"label"`
		Fields []json.RawMessage `json:"fields"`
	} `json:"entity,omitempty"`
	Field *struct {
		Entity string `json:"entity"`
	} `json:"field,omitempty"`
	FieldRaw json.RawMessage `json:"-"`
}

// applyAndProbe applies the draft's backbone and then asks the server whether the result is usable.
// It ALWAYS cleans up what it created, including on failure.
func applyAndProbe(client *http.Client, base, token string, draft json.RawMessage, spec probeSpec) (bool, string) {
	var raws []json.RawMessage
	if err := json.Unmarshal(draft, &raws); err != nil {
		return false, "probe: draft is not a list"
	}
	items := make([]draftFull, 0, len(raws))
	for _, r := range raws {
		var d draftFull
		if err := json.Unmarshal(r, &d); err != nil {
			continue
		}
		// Keep the per-item raw so a `field` draft forwards its WHOLE body to the field API — a
		// partial re-marshal here would drop per-type option keys, which is the bug class that once
		// made ExpectEntity report false negatives.
		d.FieldRaw = r
		items = append(items, d)
	}

	res := applyResult{}
	defer func() { cleanup(client, base, token, res.Created) }()

	for _, it := range items {
		switch {
		case it.Kind == "entity" && it.Entity != nil:
			body := map[string]any{
				"name": it.Entity.Name, "label": it.Entity.Label, "description": "",
				"fields": it.Entity.Fields,
			}
			if err := postJSON(client, base+"/api/v1/admin/entities", token, body, nil); err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("create entity %s: %v", it.Entity.Name, err))
				continue
			}
			res.Created = append(res.Created, it.Entity.Name)
		case it.Kind == "field" && it.Field != nil && it.Field.Entity != "":
			// The field draft's own body IS the POST body minus the routing key; forward it whole.
			var m map[string]any
			_ = json.Unmarshal(it.FieldRaw, &m)
			fb, _ := m["field"].(map[string]any)
			if fb == nil {
				res.Failures = append(res.Failures, "field draft has no body")
				continue
			}
			ent, _ := fb["entity"].(string)
			delete(fb, "entity")
			if err := postJSON(client, base+"/api/v1/admin/entities/"+ent+"/fields", token, fb, nil); err != nil {
				res.Failures = append(res.Failures, fmt.Sprintf("add field on %s: %v", ent, err))
			}
		default:
			res.Skipped = append(res.Skipped, it.Kind)
		}
	}

	if len(res.Created) == 0 {
		return false, "probe: no entity was created — nothing to be usable" + failSuffix(res)
	}
	target := spec.Entity
	if target == "" {
		target = res.Created[0]
	}

	// 1. metadata registered
	var meta struct {
		Data []struct {
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := getJSON(client, base+"/api/v1/metadata/entities", token, &meta); err != nil {
		return false, "probe: read metadata: " + err.Error() + failSuffix(res)
	}
	found := false
	for _, e := range meta.Data {
		if e.Name == target {
			found = true
		}
	}
	if !found {
		return false, "probe: " + target + " applied but MISSING from /metadata/entities" + failSuffix(res)
	}

	// 2. the physical table takes a row
	row := spec.Row
	if row == nil {
		row = map[string]any{}
	}
	var created struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := postJSON(client, base+"/api/v1/"+target, token, row, &created); err != nil {
		return false, "probe: " + target + " exists but REFUSED a row: " + err.Error() + failSuffix(res)
	}
	if created.Data.ID == "" {
		return false, "probe: " + target + " accepted a row but returned no id" + failSuffix(res)
	}

	// 3. and gives it back
	if err := getJSON(client, base+"/api/v1/"+target+"/"+created.Data.ID, token, nil); err != nil {
		return false, "probe: " + target + " wrote a row it cannot read back: " + err.Error() + failSuffix(res)
	}

	detail := fmt.Sprintf("probe OK: %s registered · accepts a row · reads it back", target)
	if len(res.Skipped) > 0 {
		detail += fmt.Sprintf(" (KHÔNG áp: %v)", res.Skipped)
	}
	return true, detail + failSuffix(res)
}

func failSuffix(r applyResult) string {
	if len(r.Failures) == 0 {
		return ""
	}
	return " [lỗi khi áp: " + strings.Join(r.Failures, "; ") + "]"
}

// cleanup removes what the probe created, newest first. Best-effort but LOUD: a leftover entity
// makes the next run of the same case fail on a duplicate name, which reads as a model regression.
func cleanup(client *http.Client, base, token string, created []string) {
	for i := len(created) - 1; i >= 0; i-- {
		req, _ := http.NewRequest("DELETE", base+"/api/v1/admin/entities/"+created[i], nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("   ⚠️ dọn dẹp: không xoá được %s: %v\n", created[i], err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			fmt.Printf("   ⚠️ dọn dẹp: %s trả HTTP %d — tenant eval còn rác, lần chạy sau sẽ trượt vì trùng tên\n",
				created[i], resp.StatusCode)
		}
	}
}

func postJSON(client *http.Client, url, token string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	return do(client, req, out)
}

func getJSON(client *http.Client, url, token string, out any) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return do(client, req, out)
}

func do(client *http.Client, req *http.Request, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var generic struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 {
		_ = json.Unmarshal(buf.Bytes(), &generic)
		return fmt.Errorf("HTTP %d %s %s", resp.StatusCode, generic.Code, trim(generic.Error, 80))
	}
	if out != nil {
		if err := json.Unmarshal(buf.Bytes(), out); err != nil {
			return fmt.Errorf("decode: %w", err)
		}
	}
	return nil
}
