# Max Node Reference Latency Design

## Goal

Add a configurable maximum node reference latency so obviously slow nodes can be excluded from routing before they are picked.

## Configuration

- System runtime config adds `max_node_reference_latency`.
- Default value is `0s`, meaning no global latency cap. This preserves upgrade behavior.
- Platform config adds `max_node_reference_latency`.
- Platform value has three states:
  - omitted or empty string: inherit the current system runtime value.
  - `0s`: explicitly disable the cap for this platform.
  - positive duration: use the platform-specific cap.

## Latency Source

The cap uses the same reference latency already shown in node summaries: average EWMA across `latency_authorities`.

If a platform has an effective cap greater than zero, a node is routable only when it has at least one authority latency sample and the average is less than or equal to the cap.

If the effective cap is zero, existing routability behavior remains: the node must still be healthy, enabled, have egress IP, match platform filters, and have any latency record.

## Runtime Flow

- Platform routable-view evaluation applies the effective cap after existing health, regex, egress IP, region, and `HasLatency` checks.
- System config updates rebuild all platform views because inherited effective caps can change.
- Platform config updates replace and rebuild only that platform.
- Existing latency dirty notifications re-evaluate affected nodes, so nodes can enter or leave a platform as probes update EWMA values.

## API And Persistence

- Persist platform override as nullable `max_node_reference_latency_ns INTEGER`.
- `NULL` means inherit the current system runtime value.
- `0` means explicitly disable the cap for this platform.
- A positive value is the platform-specific cap in nanoseconds.
- API response returns the platform override as a string. Empty string means inherit.
- Create and patch accept empty string, `0s`, or a positive Go duration string.
- Invalid durations and negative durations return `INVALID_ARGUMENT`.

## UI

- System configuration page exposes the global default with helper text that `0s` means no cap.
- Platform create/edit forms expose the platform override with helper text that blank inherits the system default and `0s` disables the cap for that platform.
- Platform details show the configured platform value in summary facts.

## Tests

- Add platform routability tests for global cap inherited by platform, platform override, explicit platform disable, missing authority latency, and latency update re-evaluation.
- Add service/API tests for create, patch, response round trip, and validation.
- Add persistence migration/round-trip tests.
- Add focused UI type/form conversion tests only if the project has existing frontend test infrastructure for those files.
