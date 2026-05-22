package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Notifier interface {
	Notify(ctx context.Context, message string) error
}

type noopNotifier struct{}

func (noopNotifier) Notify(ctx context.Context, message string) error {
	return nil
}

type telegramNotifier struct {
	botToken string
	chatID   string
	client   *http.Client
}

func newNotifier(cfg AutoBookNotificationsConfig) (Notifier, error) {
	if cfg.Telegram.Enabled {
		token := strings.TrimSpace(os.Getenv(cfg.Telegram.BotTokenEnv))
		chatID := strings.TrimSpace(os.Getenv(cfg.Telegram.ChatIDEnv))
		if token == "" {
			return nil, fmt.Errorf("telegram bot token env %s is empty", cfg.Telegram.BotTokenEnv)
		}
		if chatID == "" {
			return nil, fmt.Errorf("telegram chat id env %s is empty", cfg.Telegram.ChatIDEnv)
		}
		return &telegramNotifier{
			botToken: token,
			chatID:   chatID,
			client:   &http.Client{Timeout: 10 * time.Second},
		}, nil
	}
	return noopNotifier{}, nil
}

func (n *telegramNotifier) Notify(ctx context.Context, message string) error {
	form := url.Values{}
	form.Set("chat_id", n.chatID)
	form.Set("text", message)
	form.Set("disable_web_page_preview", "true")

	endpoint := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", n.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("telegram notification failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
