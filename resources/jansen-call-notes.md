High level

- vector on its own are defined in the same binary
- VRL - vector remap language, allows you to do operations on a value
  - allows you to transform certain values
- component in vector, can create your own sinks/transforms
- lua - scripting language
- vector sensitive data scanner built in?
- limitations:
  - No HPA within vector - one of the weak points with vector
    - need to architect this yourself
      - customers will run vector on hundreds of thousands of pods
    - you get diminishing returns on vector with how much you scale it
  - components are part of the main vector binary so you need to choose at compile time what components to use
  - no native way to run it in kubernetes (all needs to be built out by customer)
- debug logging within vector (lookup)

OTLP - Datadog

- ways to get Otel to datadog
  - **agent** - direct otlp ingestion
  - **datadog otel collector** - embedded in the agent (great for fleet mgmt)
  - **otlp endpoint** - send it directly to your Datadog account
  - **standalone otel collector** - can package and send to datadog
- otel collectors have different ways you can run them. Default modes can promote backpressure, the alternatives can help reduce these instances i.e.: gateway mode - forwards to a centralized collector/gateway that will handle disk/memory buffers
- diskpressure - RYKV - serialize directly from a rust struct, just directly puts it on disk
  - flushes the disk every .5s OR if you hit max size

tokio - gold standard for async multi-threading library
only time events are used are for e2e acknowledgements - to guarantee

- split arrays and many events at once, the EDA can make sure everything goes where it needs to

open in cursor -

- batch notifier
- end to end
