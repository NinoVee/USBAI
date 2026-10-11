package app

import (
	"fmt"
	"strings"
)

// systemHealth is a read-only report for the Platform maintenance agent:
// drive space, which engines are running, which models are present, and
// anything that failed to start. It touches nothing, so it needs no
// approval and works whether or not computer access is on.
func (a *App) systemHealth() string {
	var b strings.Builder

	// Engines.
	a.engMu.Lock()
	chatState, chatModel, chatErr, chatWarn := a.chatState, a.chatModel.Name, a.chatErr, a.chatWarn
	chatRuntime := a.chatRuntime
	embedState, embedModel := a.embedState, a.embedModel.Name
	visionState, visionModel, visionErr := a.visionState, a.visionModel.Name, a.visionErr
	speechState, speechModel := a.speechState, a.speechModel.Name
	a.engMu.Unlock()

	b.WriteString("ENGINES\n")
	fmt.Fprintf(&b, "- Chat model: %s — %s", orNone(chatModel), chatState)
	if chatRuntime != "" {
		fmt.Fprintf(&b, " (runtime %s)", chatRuntime)
	}
	b.WriteString("\n")
	if chatErr != "" {
		fmt.Fprintf(&b, "  ⚠ error: %s\n", firstLineStr(chatErr))
	}
	if chatWarn != "" {
		fmt.Fprintf(&b, "  ⚠ warning: %s\n", firstLineStr(chatWarn))
	}
	fmt.Fprintf(&b, "- Document search (embeddings): %s — %s\n", orNone(embedModel), embedState)
	fmt.Fprintf(&b, "- Image reader: %s — %s\n", orNone(visionModel), visionState)
	if visionErr != "" && visionState != StateOff {
		fmt.Fprintf(&b, "  ⚠ %s\n", firstLineStr(visionErr))
	}
	fmt.Fprintf(&b, "- Speech to text: %s — %s\n", orNone(speechModel), speechState)

	// Models on the drive.
	var present, missing []string
	for _, m := range a.cfg.ModelsByRole("chat") {
		if a.cfg.Present(m) {
			if err := a.cfg.Check(m); err != nil {
				missing = append(missing, m.Name+" (damaged: "+firstLineStr(err.Error())+")")
			} else {
				present = append(present, m.Name)
			}
		} else {
			missing = append(missing, m.Name+" (not downloaded)")
		}
	}
	b.WriteString("\nCHAT MODELS ON THE DRIVE\n")
	if len(present) > 0 {
		fmt.Fprintf(&b, "- Ready: %s\n", strings.Join(present, "; "))
	}
	if len(missing) > 0 {
		fmt.Fprintf(&b, "- Not usable: %s\n", strings.Join(missing, "; "))
	}

	// Storage.
	if r, err := a.Storage(); err == nil {
		b.WriteString("\nSTORAGE\n")
		fmt.Fprintf(&b, "- Drive: %s free of %s (%s used)\n", humanSizeGB(r.Free), humanSizeGB(r.Total), humanSizeGB(r.Total-r.Free))
		if r.Total > 0 {
			pct := float64(r.Total-r.Free) / float64(r.Total) * 100
			fmt.Fprintf(&b, "- Drive is %.0f%% full", pct)
			switch {
			case pct >= 95:
				b.WriteString(" — ⚠ critically full; free space before adding models or files.\n")
			case pct >= 85:
				b.WriteString(" — ⚠ getting full.\n")
			default:
				b.WriteString(".\n")
			}
		}
		for _, k := range []string{"models", "files", "chats", "memory", "agents", "app", "data", "other"} {
			if n := r.Parts[k]; n > 0 {
				fmt.Fprintf(&b, "  • %s: %s\n", k, humanSizeGB(n))
			}
		}
		if r.Cache > 0 {
			fmt.Fprintf(&b, "- Model cache on this computer (not the drive): %s\n", humanSizeGB(r.Cache))
		}
		if r.Locked {
			b.WriteString("- The vault is locked, so per-category sizes for your own data aren't included.\n")
		}
	} else {
		fmt.Fprintf(&b, "\nSTORAGE\n- could not read drive space: %s\n", firstLineStr(err.Error()))
	}

	// Overall note for the agent to reason about.
	b.WriteString("\nNo backup system or background jobs exist in this app, so there are no failed jobs or backups to report.")
	return b.String()
}

func firstLineStr(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// humanSizeGB formats bytes as GB or MB for a health report.
func humanSizeGB(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n/(1<<10))
	}
}
