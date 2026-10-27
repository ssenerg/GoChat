package render

import (
	"fmt"
	"html/template"
)

// icons are the interface's line icons: drawn on a 24px grid, stroked with
// the current text color so they follow every theme.
var icons = map[string]string{
	"menu":           `<path d="M4 7h16M4 12h16M4 17h16"/>`,
	"close":          `<path d="M6 6l12 12M18 6L6 18"/>`,
	"chevron":        `<path d="M6 9l6 6 6-6"/>`,
	"plus":           `<path d="M12 5v14M5 12h14"/>`,
	"search":         `<circle cx="11" cy="11" r="6.5"/><path d="M16 16l4 4"/>`,
	"more":           `<circle cx="6" cy="12" r="1.4" fill="currentColor" stroke="none"/><circle cx="12" cy="12" r="1.4" fill="currentColor" stroke="none"/><circle cx="18" cy="12" r="1.4" fill="currentColor" stroke="none"/>`,
	"settings":       `<path d="M4 6.5h8.5M16.5 6.5H20M4 12h3.5M11.5 12H20M4 17.5h10.5M18.5 17.5H20"/><circle cx="14.5" cy="6.5" r="2"/><circle cx="9.5" cy="12" r="2"/><circle cx="16.5" cy="17.5" r="2"/>`,
	"sign-out":       `<path d="M10 4H7a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h3M15 8l4 4-4 4M19 12H10"/>`,
	"back":           `<path d="M19 12H5M11 18l-6-6 6-6"/>`,
	"hash":           `<path d="M5 9h14M5 15h14M10.5 4L8.5 20M15.5 4l-2 16"/>`,
	"lock":           `<rect x="5" y="11" width="14" height="9" rx="2"/><path d="M8.5 11V8a3.5 3.5 0 0 1 7 0v3"/>`,
	"members":        `<circle cx="9" cy="8.5" r="3.5"/><path d="M3 20a6 6 0 0 1 12 0M15.5 5.2a3.5 3.5 0 0 1 0 6.6M17.5 14.4A6 6 0 0 1 21 20"/>`,
	"invite":         `<circle cx="9" cy="8.5" r="3.5"/><path d="M3 20a6 6 0 0 1 12 0M19 8v6M16 11h6"/>`,
	"leave":          `<path d="M14 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3M9 8l-4 4 4 4M5 12h10"/>`,
	"attach":         `<path d="M20 11.5l-7.6 7.6a5 5 0 0 1-7.1-7.1l7.9-7.9a3.4 3.4 0 0 1 4.8 4.8l-7.9 7.9a1.7 1.7 0 0 1-2.4-2.4l7.2-7.2"/>`,
	"send":           `<path d="M5 12L3.5 4.5 21 12 3.5 19.5 5 12zM5 12h7"/>`,
	"edit":           `<path d="M4 20h4L19 9a2.8 2.8 0 0 0-4-4L4 16v4zM13.5 6.5l4 4"/>`,
	"trash":          `<path d="M4 7h16M9.5 7V4.5h5V7M6.5 7l.8 11.2A2 2 0 0 0 9.3 20h5.4a2 2 0 0 0 2-1.8L17.5 7M10 11v5M14 11v5"/>`,
	"download":       `<path d="M12 4v11M7.5 10.5L12 15l4.5-4.5M5 20h14"/>`,
	"photo":          `<rect x="3.5" y="5" width="17" height="14" rx="2.5"/><circle cx="9" cy="10" r="1.75"/><path d="M20.5 15.5l-4.5-4.5-8.5 8"/>`,
	"check":          `<path d="M5 12.5l4.5 4.5L19 7.5"/>`,
	"bell":           `<path d="M6.5 16.5V11a5.5 5.5 0 0 1 11 0v5.5l1.5 1.5H5l1.5-1.5zM10 20.5a2.2 2.2 0 0 0 4 0"/>`,
	"bell-off":       `<path d="M9 5.9A5.5 5.5 0 0 1 17.5 11v4M6.5 9.6V16.5L5 18h12M10 20.5a2.2 2.2 0 0 0 4 0M4 4l16 16"/>`,
	"smile":          `<circle cx="12" cy="12" r="8.5"/><path d="M8.6 14.2a4.2 4.2 0 0 0 6.8 0"/><circle cx="9.3" cy="10" r="1.1" fill="currentColor" stroke="none"/><circle cx="14.7" cy="10" r="1.1" fill="currentColor" stroke="none"/>`,
	"pin":            `<path d="M9 3.5h6M10 3.5l-.7 5.6L6 12.5V14h12v-1.5l-3.3-3.4L14 3.5M12 14v6.5"/>`,
	"bookmark":       `<path d="M7 3.5h10a1 1 0 0 1 1 1V20.5l-6-4.2-6 4.2V4.5a1 1 0 0 1 1-1z"/>`,
	"reply":          `<path d="M10 7L5 12l5 5"/><path d="M5 12h8.5a5.5 5.5 0 0 1 5.5 5.5V19"/>`,
	"mic":            `<rect x="9" y="3.5" width="6" height="11" rx="3"/><path d="M5.5 11.5a6.5 6.5 0 0 0 13 0M12 18v2.5M9 20.5h6"/>`,
	"play":           `<path d="M8.5 5.8v12.4a.8.8 0 0 0 1.2.7l9.6-6.2a.8.8 0 0 0 0-1.4L9.7 5.1a.8.8 0 0 0-1.2.7z" fill="currentColor"/>`,
	"pause":          `<rect x="7" y="5.5" width="3.5" height="13" rx="1" fill="currentColor"/><rect x="13.5" y="5.5" width="3.5" height="13" rx="1" fill="currentColor"/>`,
	"mic-off":        `<rect x="9" y="3.5" width="6" height="11" rx="3"/><path d="M5.5 11.5a6.5 6.5 0 0 0 13 0M12 18v2.5M9 20.5h6M4 4l16 16"/>`,
	"speaker":        `<path d="M4.5 9.5H8l4.5-4v13L8 14.5H4.5z"/><path d="M15.5 9a4 4 0 0 1 0 6M18 6.5a7.5 7.5 0 0 1 0 11"/>`,
	"headphones":     `<path d="M4.5 15v-3a7.5 7.5 0 0 1 15 0v3"/><rect x="3.5" y="14" width="4" height="6" rx="1.5"/><rect x="16.5" y="14" width="4" height="6" rx="1.5"/>`,
	"headphones-off": `<path d="M4.5 15v-3a7.5 7.5 0 0 1 15 0v3"/><rect x="3.5" y="14" width="4" height="6" rx="1.5"/><rect x="16.5" y="14" width="4" height="6" rx="1.5"/><path d="M3 3l18 18"/>`,
	"phone-off":      `<path d="M3.5 13.5c4.6-4 12.4-4 17 0l-2 2.6-3.2-1.2v-2.4a10.5 10.5 0 0 0-6.6 0v2.4l-3.2 1.2z"/>`,
	"signal":         `<path d="M5 19v-2.5M9.5 19v-5.5M14 19v-8.5M18.5 19V6"/>`,
	"video":          `<rect x="3" y="6.5" width="12.5" height="11" rx="2.5"/><path d="M15.5 10.5l5-3v9l-5-3"/>`,
	"video-off":      `<rect x="3" y="6.5" width="12.5" height="11" rx="2.5"/><path d="M15.5 10.5l5-3v9l-5-3M3 3l18 18"/>`,
	"screen":         `<rect x="3" y="4" width="18" height="12.5" rx="2"/><path d="M8 20.5h8M12 16.5v4M12 13V7.5M9.5 10l2.5-2.5 2.5 2.5"/>`,
	"screen-off":     `<rect x="3" y="4" width="18" height="12.5" rx="2"/><path d="M8 20.5h8M12 16.5v4M3 3l18 18"/>`,
	"expand":         `<path d="M4 9V5a1 1 0 0 1 1-1h4M15 4h4a1 1 0 0 1 1 1v4M20 15v4a1 1 0 0 1-1 1h-4M9 20H5a1 1 0 0 1-1-1v-4"/>`,
	"browse":         `<path d="M4 6h16M4 12h10M4 18h7"/><circle cx="17.5" cy="16.5" r="2.5"/><path d="M19.5 18.5L21 20"/>`,
}

// Icon renders a named icon for a template. Templates only name known icons,
// which a test checks.
func Icon(name string) (template.HTML, error) {
	body, ok := icons[name]
	if !ok {
		return "", fmt.Errorf("unknown icon %q", name)
	}
	return template.HTML(`<svg class="i" viewBox="0 0 24 24" aria-hidden="true" focusable="false" fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round">` + body + `</svg>`), nil //nolint:gosec // fixed markup
}
