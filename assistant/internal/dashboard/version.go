package dashboard

// APIVersion is the Dashboard view-shape contract. Bump it whenever a view
// struct's meaning changes in a way an older UI would misread (a renamed or
// removed field, a changed unit or enum). The value is generated into
// web/src/api-version.ts and checked by a Go test, and the session view carries
// it so a UI built against another version shows a visible mismatch page
// instead of rendering undefined fields.
const APIVersion = 1
