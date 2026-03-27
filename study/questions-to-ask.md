# Questions to Ask the STACKIT Telemetry Router Team
### Interview Prep — Mike Falcone

---

## Why Good Questions Matter Here

STACKIT interviewers will judge your questions as a signal of:
- **Product depth** — do you understand what the Telemetry Router actually does day-to-day?
- **SRE curiosity** — do you think about operations, not just features?
- **Sovereignty awareness** — do you understand what makes STACKIT different from a hyperscaler?

Don't ask anything whose answer is on the public docs page. Ask things only someone on the team can answer.

---

## Category 1: Technical Architecture (ask 2–3)

**"The Telemetry Router is built on Vector — how much of Vector's behavior do you extend with custom plugins vs. configure with TOML? Have you hit limits in VRL that required a different approach?"**

*Why it's good:* Shows you know Vector deeply enough to know its limits. It signals you won't oversell what VRL can do — a real practitioner concern.

---

**"For the fan-out to multiple destinations — when a customer has 5 sinks and one of them is consistently slow, how is that currently surfaced to the customer vs. to the SRE team? Are those the same signal or different?"**

*Why it's good:* Probes whether the observability of the router itself is mature. It also shows you think about the two-audience problem: customers want a health status, SREs want raw metrics.

---

**"The Audit Log service is being migrated to the Telemetry Router right now. What's been the hardest migration challenge — schema compatibility, delivery guarantees, or something else entirely?"**

*Why it's good:* Only someone on the team can answer this. It shows you read the docs and understand the current moment. Also reveals real engineering challenges you'd be walking into.

---

**"How does the operator handle the case where a customer pushes a Vector config change that causes Vector to fail to start — does the operator detect the crash loop and roll back, or is that left to the customer to notice?"**

*Why it's good:* This is a real operational edge case. It shows you think about failure modes in the reconcile loop, not just the happy path.

---

## Category 2: Scale & Performance (ask 1)

**"What does a spike look like in production for the Telemetry Router — is it bursty by customer (one customer's k8s cluster restarting) or correlated across customers (a STACKIT region event)? How does the current architecture handle the difference?"**

*Why it's good:* Shows you think about multi-tenant load patterns, not just single-tenant sizing. The answer will tell you a lot about how mature their capacity planning is.

---

## Category 3: Team & Process (ask 1–2)

**"You Build It You Run It — what does on-call actually look like for this team? Is it the feature engineers rotating through, or is there a dedicated SRE layer?"**

*Why it's good:* Direct, honest question about operational reality. Every YBYRI team has a different actual implementation. Shows you take it seriously.

---

**"The JD mentions 'identifying bottlenecks and implementing sustainable fixes.' Can you give me an example of a bottleneck you found recently in the pipeline — something that wasn't obvious until you dug into the telemetry?"**

*Why it's good:* Inverts the usual flow — you're asking them to demonstrate the debugging process you'd be doing. It shows you're thinking about the job, not the interview.

---

**"Where is the team currently investing in testing — unit tests for the operator reconcile logic, integration tests with a real k8s cluster, load/chaos testing? What's the biggest testing gap right now?"**

*Why it's good:* Shows engineering maturity awareness. The honest answer about gaps will tell you what kind of codebase you're joining.

---

## Category 4: Product Roadmap (ask 1)

**"The docs say the initial release is OTLP-only for audit logs. The name 'Telemetry Router' implies broader ambition — metrics and traces eventually. What's the sequencing there, and what's the hardest part of extending to metrics specifically?"**

*Why it's good:* Shows you read the product positioning carefully. Metrics are architecturally very different from logs (aggregation, cardinality, push vs. pull) — asking about this signals technical depth.

---

## Category 5: The Closing Question (always ask this last)

**"What would make someone successful in this role in the first 90 days — and what's something that trips up new engineers on this team that I should know going in?"**

*Why it's good:* Gives the interviewer a chance to be honest with you. The second half of the question — "what trips up new engineers" — is a rare invitation to give real advice rather than a polished pitch. The best interviewers will tell you something genuinely useful.

---

## Questions NOT to Ask

Avoid these — they're either on the docs or will make you look underprepared:

- "What tech stack do you use?" (it's in the JD: Go, k8s, Vector, OTLP)
- "How many customers do you have?" (STACKIT won't disclose this in an interview)
- "Is there remote work?" (ask HR, not the engineering panel)
- "What does the Telemetry Router do?" (you should already know)

---

## Logistics

Save 5–10 minutes at the end for questions. Pick **3–4** from the list above — you won't have time for all of them. Prioritize:

1. One architecture question (shows technical depth)
2. One migration/current-state question (shows situational awareness)
3. One team/process question (shows "You Build It You Run It" seriousness)
4. The closing question (always)
