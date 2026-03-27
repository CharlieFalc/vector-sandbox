# Go REST API Coding Challenge
### STACKIT Telemetry Router — Practice Cold

---

## How to Use This

This is a **fresh challenge** designed to be attempted cold — without referencing the `telemetry-router/` implementation we already built. The scenario is deliberately parallel but different enough that you can't copy-paste.

**Time target:** 45–60 minutes
**Language:** Go (preferred for STACKIT)
**What you'll be evaluated on:** Code structure, error handling, async pattern, mock services, HTTP status code reasoning

---

## The Scenario

STACKIT's Telemetry Router needs a new sub-service: a **Transform Registry**. Customers can register named VRL transform programs that the router applies before fan-out. Think of it as a library of reusable PII-redaction scripts.

---

## Requirements

### Part 1 — POST /transforms (20 min)

Implement the `POST /transforms` endpoint.

**Request body:**
```json
{
  "name": "redact-emails",
  "description": "Strips email addresses from log bodies",
  "vrl": ".message = replace(.message, r'\\b[\\w.]+@[\\w.]+\\b', \"[REDACTED]\")"
}
```

**Behavior:**
- Validate that `name` and `vrl` are non-empty
- Generate a unique `transformId`
- Return `201 Created` with:
```json
{
  "transformId": "tr-abc123",
  "name": "redact-emails",
  "status": "COMPILING",
  "createdAt": "2026-03-22T14:00:00Z"
}
```
- Immediately trigger a background goroutine that **simulates VRL compilation** (sleep 2–4 seconds, then randomly succeed or fail at a 20% failure rate)
- On success: status → `ACTIVE`; on failure: status → `FAILED` with an `errorMessage`

**Status codes to implement:**
- `201 Created` — new transform initiated
- `400 Bad Request` — missing/invalid fields
- `409 Conflict` — a transform with this `name` already exists

---

### Part 2 — GET /transforms/{transformId} (10 min)

Implement the status polling endpoint.

**Response (ACTIVE):**
```json
{
  "transformId": "tr-abc123",
  "name": "redact-emails",
  "status": "ACTIVE",
  "createdAt": "2026-03-22T14:00:00Z",
  "compiledAt": "2026-03-22T14:00:03Z"
}
```

**Response (FAILED):**
```json
{
  "transformId": "tr-abc123",
  "name": "redact-emails",
  "status": "FAILED",
  "errorMessage": "VRL compilation error: unexpected token at line 1",
  "createdAt": "2026-03-22T14:00:00Z"
}
```

- `200 OK` — found (regardless of status)
- `404 Not Found` — transform doesn't exist

---

### Part 3 — DELETE /transforms/{transformId} (10 min)

Implement deletion with a constraint: you cannot delete an `ACTIVE` transform that is referenced by any router instance.

**Mock a `checkTransformInUse(transformId string) bool`** function that randomly returns `true` 30% of the time.

- If in use: `409 Conflict` with body `{"code": 409, "message": "transform is referenced by 1 or more router instances"}`
- If not in use: delete from store and return `204 No Content`
- If not found: `404 Not Found`

---

### Part 4 — Bonus (if time allows)

Implement `GET /transforms` with optional query parameter filtering:

- `?status=ACTIVE` — return only active transforms
- `?name=redact` — return transforms whose name contains the substring (case-insensitive)

---

## Constraints

- **Standard library only** — no gorilla/mux, no chi, no gin. Use `net/http`
- **In-memory store only** — a `map[string]*Transform` protected by a `sync.RWMutex`
- No real VRL compilation — mock it with a sleep + random outcome
- Must handle concurrent requests safely

---

## What the Evaluator Is Looking For

| Area | What they want to see |
|---|---|
| HTTP status codes | Correct use of 201 vs 202 vs 200 vs 204 vs 400 vs 404 vs 409 |
| Async pattern | Goroutine launched from handler; result written back to in-memory store |
| Concurrency safety | `sync.RWMutex` on reads, full `Lock` on writes |
| Error handling | No panics; all errors return structured JSON, not plain text |
| Code structure | Types separated from handlers; store as its own concern |
| Naming | Go idioms: `transformID` not `transform_id`, receiver methods, etc. |

---

## Hints (only read if stuck)

<details>
<summary>Hint 1: Background compilation goroutine</summary>

```go
go func(id string) {
    // simulate compilation time
    time.Sleep(time.Duration(2+rand.Intn(3)) * time.Second)

    if rand.Float32() < 0.2 { // 20% failure
        store.UpdateStatus(id, "FAILED", "VRL compilation error: unexpected token at line 1")
        return
    }
    store.UpdateStatus(id, "ACTIVE", "")
}(transformID)
```
</details>

<details>
<summary>Hint 2: Path variable extraction without a router library</summary>

```go
// URL: /transforms/tr-abc123
// r.URL.Path = "/transforms/tr-abc123"
parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
// parts = ["transforms", "tr-abc123"]
if len(parts) != 2 {
    http.NotFound(w, r)
    return
}
transformID := parts[1]
```
</details>

<details>
<summary>Hint 3: 409 Conflict for duplicate name</summary>

```go
// In your store:
func (s *Store) GetByName(name string) *Transform {
    s.mu.RLock()
    defer s.mu.RUnlock()
    for _, t := range s.transforms {
        if t.Name == name {
            return t
        }
    }
    return nil
}

// In your handler:
if existing := h.store.GetByName(req.Name); existing != nil {
    writeError(w, http.StatusConflict, "a transform with this name already exists")
    return
}
```
</details>

---

## Self-Evaluation Checklist

After you finish, check off:

- [ ] `POST /transforms` returns `201` with `COMPILING` status immediately
- [ ] Background goroutine updates status to `ACTIVE` or `FAILED` after 2–4 seconds
- [ ] `GET /transforms/{id}` returns current status (poll it and watch it change)
- [ ] `DELETE /transforms/{id}` returns `409` when in-use, `204` when deleted
- [ ] All error responses are `{"code": N, "message": "..."}` JSON, not plain text
- [ ] Concurrent requests don't race (your mutex covers all map reads and writes)
- [ ] You used `201` for creation, not `200`
- [ ] You can explain why DELETE returns `204 No Content` instead of `200 OK`

---

## The Explanation They Want to Hear for 201 vs 202

In this challenge, `POST /transforms` returns **`201 Created`** (not `202 Accepted`) because the transform *record* is created synchronously in the database — the resource exists immediately and has a stable `transformId`. The fact that it's still compiling is reflected in the `status: "COMPILING"` field. The client has everything they need to poll for completion.

`202 Accepted` would be appropriate if the API *itself* couldn't even confirm the record was created — e.g., if creation was delegated to a queue and the API didn't know the resulting ID yet. In the Telemetry Router's destination management API, `PUT /destinations/{id}` returns `202` because the actual work (operator reconcile → Vector config update) is fully async and the API doesn't know when it will complete.

**The rule:** `201` when the resource exists in your DB. `202` when you can't even tell the client the resource's stable ID yet.
