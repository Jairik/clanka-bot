// The Dashboard view-shape contract this UI was built against. It must equal
// internal/dashboard/version.go's APIVersion; a Go test enforces that, and the
// session view's value is compared at runtime so a mismatched artifact fails
// visibly instead of rendering undefined fields.
export const API_VERSION = 1;
