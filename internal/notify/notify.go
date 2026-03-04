package notify

import (
	"log"
	"os/exec"
	"strings"
)

func Send(title, body string) {
	cmd := exec.Command("notify-send", "--app-name=VibeProxy", title, body)
	if err := cmd.Run(); err != nil {
		log.Printf("[Notify] Failed to send notification: %v", err)
	}
}

func CopyToClipboard(text string) error {
	cmd := exec.Command("wl-copy", text)
	if err := cmd.Run(); err != nil {
		cmd = exec.Command("xclip", "-selection", "clipboard")
		cmd.Stdin = strings.NewReader(text)
		return cmd.Run()
	}
	return nil
}

func OpenURL(url string) {
	cmd := exec.Command("xdg-open", url)
	if err := cmd.Start(); err != nil {
		log.Printf("[Notify] Failed to open URL %s: %v", url, err)
		return
	}
	go cmd.Wait()
}
