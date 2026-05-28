package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// ctrlV is the byte produced by Ctrl+V (SYN). Claude Code reads the system
// clipboard (and attaches any image it finds) when it receives this.
const ctrlV = 0x16

// PasteImage delivers a pasted image to the program running in the PTY.
//
// Primary path: write the image to the server's system clipboard and send
// Ctrl+V so a clipboard-aware program (e.g. Claude Code) reads it.
//
// Fallback (no display / no clipboard tool): save the image to a file under
// storeDir and type its absolute path into the terminal, so the program can
// read the image from disk.
func (s *Session) PasteImage(data []byte, mime, storeDir string) error {
	if len(data) == 0 {
		return errors.New("empty image data")
	}
	if mime == "" {
		mime = "image/png"
	}

	if err := copyImageToClipboard(data, mime); err == nil {
		return s.WriteInput([]byte{ctrlV})
	} else {
		log.Printf("session %s: clipboard unavailable (%v), falling back to file path", s.ID, err)
	}

	path, err := saveImageFile(data, mime, storeDir, s.ID)
	if err != nil {
		return fmt.Errorf("save pasted image: %w", err)
	}
	// Trailing space delimits the path from anything typed next.
	return s.WriteInput([]byte(path + " "))
}

// copyImageToClipboard pushes image bytes onto the system clipboard using
// wl-copy (Wayland) or xclip (X11). Returns an error if no display server or
// clipboard tool is available.
func copyImageToClipboard(data []byte, mime string) error {
	wayland := os.Getenv("WAYLAND_DISPLAY") != ""
	x11 := os.Getenv("DISPLAY") != ""
	if !wayland && !x11 {
		return errors.New("no display available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	switch {
	case wayland && hasBin("wl-copy"):
		cmd = exec.CommandContext(ctx, "wl-copy", "--type", mime)
	case x11 && hasBin("xclip"):
		cmd = exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", mime, "-i")
	default:
		return errors.New("no clipboard tool (install wl-clipboard or xclip)")
	}

	// Leave Stdout/Stderr nil (routed to /dev/null). These tools fork a
	// background process to own the selection; if it inherited our pipes,
	// cmd.Wait would block on them until the daemon exits.
	cmd.Stdin = bytes.NewReader(data)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	return nil
}

func saveImageFile(data []byte, mime, storeDir, id string) (string, error) {
	dir := filepath.Join(storeDir, id, "pastes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("paste-%d%s", time.Now().UnixNano(), extForMime(mime))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, nil
	}
	return abs, nil
}

func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func extForMime(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	default:
		return ".png"
	}
}
