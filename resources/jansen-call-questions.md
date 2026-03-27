# Questions for Jansen — Friday Call
### STACKIT Telemetry Router Interview Prep

---

## Priority Questions (lead with these)

**1. The multi-tenant backpressure problem**
> "In a setup where you've got many independent pipelines running — whether that's one per customer or one per data type — how do you prevent one slow or degraded sink from starving the whole topology? Is the answer disk buffers, separate worker processes, or something else? And what's the real-world throughput cost of disk buffering vs. in-memory?"

---

**2. Vector upgrades without dropping events**
> "How do you handle Vector version upgrades without losing in-flight events? Does the disk buffer actually give you a clean handoff, or is there always a gap? Do you drain before kill, or do you rely on end-to-end acknowledgements from the source side?"

---

**3. What VRL actually can't do**
> "Where have you hit the wall with VRL — things customers wanted to transform that you just couldn't express in VRL without getting ugly? Did you end up with custom WASM transforms, Lua, or just telling the customer no?"

---

**4. How you observe Vector itself**
> "What internal metrics do you actually watch on Vector to know it's healthy — not the pipeline outputs, but the Vector process itself? Things like buffer utilization, component error rates, dropped event counters? And is there a moment where those internal metrics start lying to you?"

---

**5. The poison payload problem**
> "What happens when a malformed or oversized event hits a VRL transform that panics — does Vector isolate it per-event, or does it tank the whole component? How do you handle poison payloads in a high-volume pipeline without an operator spending half their day clearing dead-letter queues?"

---

**6. The one thing you'd do differently (closing question)**
> "If you were starting the DD OPs worker from zero today, knowing what you know now — what's the one architectural decision you'd make differently? Specifically around how the control plane talks to Vector, or how configuration changes are applied without drops."

---

**7. What his team actually tests for in interviews**
> "When your team interviews engineers for a role like this, what do you actually probe for — is it mostly systems design around pipeline topology, or do you go deep on Vector/VRL specifics, or something else? I want to make sure I'm studying the right things this week."

---

## Notes
- Keep it on Vector internals and pipeline topology — skip DD-specific stuff (Agent integration, log archive/rehydration, pricing)
- 30 min goes fast — hit Q1 and Q6 at minimum
- His buffering/backpressure doc: https://docs.datadoghq.com/observability_pipelines/scaling_and_performance/buffering_and_backpressure/
