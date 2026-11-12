# One SQLite database, owned by the assistant process, holds all persistent state

Scheduled alerts, settings overrides, lists and notes, and Hermes job state all live in a single SQLite file on a Docker volume. Only the Go `assistant` process opens it, using a pure-Go driver so the arm64 image needs no cgo. Other services (the Dashboard, Hermes) reach this state through `assistant`'s API, never by opening the file. We chose this so restarts and deploys keep state, so there is one place to back up, and so the rules about who may change what (Permission levels, Local-only data) are enforced in one process. Settings resolve in three layers: code default, then `config.yaml`, then a stored override that can be reset.

## Considered Options

- **Several files (`config.local.yaml`, JSON for timers):** rejected. Each file needs its own atomic-write and migration story, and concurrent writes from the Dashboard and voice Tools become a locking problem.
- **SQLite plus a YAML override file:** rejected. Overrides would live in two places with no single answer to "what is the effective value?".
- **Each service keeps its own store:** rejected for Map 2. Hermes may keep private working memory of its own, but anything the Assistant's own features depend on lives in this database.
