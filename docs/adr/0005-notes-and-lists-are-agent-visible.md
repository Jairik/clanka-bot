# Notes and lists are agent-visible; deleting a note needs a Confirmation

The owner's answer on #32 changes the default for the personal Notes and the fixed Shopping and To-do Lists. These Tools are `LocalOnlyNo`, so the Agent path (and later Hermes, through the same registry) may read, add and list them when the user's own request calls for it. This is a deliberate, narrow exception to "personal data stays on the Pi", not a change to the egress rule: every value still leaves only through the single prompt builder ([ADR-0002](0002-single-prompt-builder-is-the-only-egress.md)) inside a request the user made. Nothing is pushed in the background.

Why: the owner wants the assistant to interact with their lists and notes, such as "add that to my shopping list" during an Agent turn, and Hermes may need the same later. Making them Local-only would hide the Tools from the Agent path and from MCP, which would break that intent.

`note.forget` is `confirm` and `AgentOnly`: deleting a note is hard to undo, so the model must ask through a spoken Confirmation. The semantic Fast path never offers it. The Dashboard deletes only after an explicit owner action, behind the same identity and same-origin checks as every other page.

## Consequences

- The Note and List text can appear in an Agent model request. The user asks for that interaction; it is not a background leak. ADR-0002's prompt builder still drops Local-only parts from the same turn.
- Speech, logs and the Activity page keep the existing rules: the text is spoken or shown locally, and it is never written to a log line.
- The Dashboard remains the owner's direct edit surface: it reads and writes the same managers as the voice and Agent paths.
- If a future decision makes them Local-only again, both modules only change their two `LocalOnly` declarations and the ADR is superseded; the storage and Tools stay the same.
