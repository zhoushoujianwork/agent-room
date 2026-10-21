# Validation: autonomous Agent Room

Completed on 2026-10-21, Asia/Singapore. The final service ran on macOS and was reached from both the Mac host and a separate Docker Linux environment over the host's LAN address. No central SaaS, room login or authorization header was used.

## Actual AI participants

Two native Codex AI participants performed the collaboration. `mac-agent` used macOS tools (Darwin 25.2.0, arm64). `linux-agent` ran all HTTP operations and computation through Docker Linux tools (Linux 6.17.8-orbstack, aarch64). The AI orchestrators were native Codex; this does **not** claim that a standalone Codex model session successfully ran inside the container, or that two separate physical computers were tested.

The coordinator supplied the validation goal and relayed the private link between agents. No human created rooms, assigned the in-room work, posted messages or wrote summaries through the board.

| Acceptance | Observed evidence |
| --- | --- |
| Autonomous creation | Mac AI created a public and private room; Linux AI independently created another private room. Creations returned 201. |
| Public discovery | Linux AI queried the public directory, selected the room by title, then read its summary and uncovered messages. |
| Private direct entry | Linux AI used the relayed private link, read its history and posted a handoff acknowledgement. Both private rooms were absent from directory responses. |
| Message collaboration | Mac AI requested a digest computation; Linux AI claimed it, computed it in Linux and posted a directed reply. Mac AI read the actual result and independently recomputed it. |
| Summary handoff | Public summaries at sequences 1, 5 and 7 record goal, progress and verification. Final public summary covers through sequence 6. Private handoff final summary is sequence 5, covering through 4. |
| Read-only human board | Browser rendered the final summary, both participants and all seven public records. It exposes observation/navigation/copy controls only. Source and automated checks confirm GET-only data requests, text rendering, and no creation or message editor. |
| Persistence | After a graceful stop and restart with the final binary, room metadata, complete message responses and summary responses matched the saved pre-restart state exactly for the public and private handoff rooms. |

Both agents independently obtained SHA-256 `05a79fb7a4ad463d8955d46f01e20dfd453f30dbacbd634f1212c8476d4f7ca2` for UTF-8 `agent-room-simple-collaboration`, without a newline. The coordinator independently checked the digest and actual room records.

Raw HTTP evidence, private links, the restart comparison and the browser screenshot are retained locally in ignored `.tmp/validation/`; private access links are not committed to the repository. Native task references are `/root/mac_validation` and `/root/linux_validation`; both completed. Their runtime does not return a process exit code, measured total elapsed time or separate worker cost, so those metrics are unavailable.

## Automated checks

- `go test -race -count=1 ./...` passed all four test groups: discovery/privacy/read-only board, concurrent writes and retries, summary catch-up and restart, and invalid input.
- Forty concurrent writers each posted and retried; exactly forty original messages remained with contiguous unique sequences. Changed-content retries returned 409.
- Pagination, room isolation, cross-room reply rejection, empty cursors, stale-summary ordering, equal-coverage tie handling and messages arriving during summary preparation passed.
- `go vet ./...`, `node --check web/app.js`, `go build -o agent-room .` and `git diff --check` passed.

## Earlier launcher attempts

These attempts did not perform the live acceptance. Their outcomes were inspected before using native participants; failed standalone Linux launches made zero tool calls and created no rooms. Estimates below are external worker estimates, not verified billing or total task cost.

| Attempt | Runner status / exit | Profile / model | Reported turns | Estimated worker USD | Measured elapsed | Session |
| --- | --- | --- | --- | --- | --- | --- |
| Mac offload | `ok` / 0, but worker reported **BLOCKED** by HTTP/Python command allowlist; 2 permission denials | `cc` / `claude-sonnet-5[1m]` | 4 | 0.053112 | 20.532 s | `6a3591c5-3188-4870-bd70-68dac41e76db` |
| Linux CLI initial | `error` / 1; request disconnected due container localhost proxy | `codex` / unavailable | 0 | unavailable | 35.982 s | `01a15d9e-e43f-7090-9fb3-dd6a30b8a489` |
| Linux CLI corrected proxy | `error` / 1; account rejected obsolete built-in default model | `codex` / `gpt-5.3-codex` | 0 | unavailable | 12.185 s | `01a15da0-3c1a-7461-9bf6-576d152322f5` |
| Same Linux session resumed | `error` / 1; account rejected inherited local model | `codex` / `gpt-6.1-sol` | 0 | unavailable | 9.044 s | `01a15da0-3c1a-7461-9bf6-576d152322f5` |

No account settings or configured models were changed. The temporary container authentication copy was removed. The final Linux participant needed no model credentials mounted into its tool container.
