package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	PollInterval    = 3 * time.Second
	CoinsPerRequest = 48
	TotalCoins      = 480
	RequestTimeout  = 8 * time.Second
)

// Config from environment
var (
	TelegramBotToken = getEnv("TELEGRAM_BOT_TOKEN", "8470861101:AAG79HG58KZSKnlG6dU3m-cSRDvreNXzm6o")
	TelegramChatID   = getEnv("TELEGRAM_CHAT_ID", "7126127814")
)

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

type Coin struct {
	Mint         string  `json:"mint"`
	Name         string  `json:"name"`
	Symbol       string  `json:"symbol"`
	IsHackathon  bool    `json:"is_hackathon"`
	USDMarketCap float64 `json:"usd_market_cap"`
	Twitter      string  `json:"twitter"`
	Website      string  `json:"website"`
}

type TelegramUpdate struct {
	UpdateID int `json:"update_id"`
	Message  *struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
			Type  string `json:"type"`
		} `json:"chat"`
		Text string `json:"text"`
		From struct {
			Username string `json:"username"`
		} `json:"from"`
	} `json:"message"`
}

type TelegramResponse struct {
	OK     bool             `json:"ok"`
	Result []TelegramUpdate `json:"result"`
}

var (
	proxies          []string
	proxyIndex       uint64
	knownHackathon   = make(map[string]bool)
	knownHackathonMu sync.RWMutex
	isFirstScan      = true
	scanCount        uint64
	lastUpdateID     int
	alertChatID      string
)

func loadProxies() {
	// Use hardcoded proxies
	proxies = hardcodedProxies
}

func getNextProxy() string {
	if len(proxies) == 0 {
		return ""
	}
	idx := atomic.AddUint64(&proxyIndex, 1)
	return proxies[idx%uint64(len(proxies))]
}

// Telegram bot command handler
func handleTelegramCommands() {
	for {
		apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=30", TelegramBotToken, lastUpdateID+1)

		resp, err := http.Get(apiURL)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var tgResp TelegramResponse
		if err := json.Unmarshal(body, &tgResp); err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		for _, update := range tgResp.Result {
			lastUpdateID = update.UpdateID

			if update.Message == nil {
				continue
			}

			text := strings.TrimSpace(update.Message.Text)
			chatID := update.Message.Chat.ID
			chatType := update.Message.Chat.Type
			chatTitle := update.Message.Chat.Title

			// Handle /chatid command
			if strings.HasPrefix(text, "/chatid") {
				var response string
				if chatType == "private" {
					response = fmt.Sprintf("🆔 Your Chat ID: `%d`\n\nUse this ID for private alerts.", chatID)
				} else {
					response = fmt.Sprintf("🆔 Group Chat ID: `%d`\n📛 Group: %s\n\nUse this ID for group alerts.", chatID, chatTitle)
				}
				sendTelegramMessage(fmt.Sprintf("%d", chatID), response)
			}

			// Handle /setalert command
			if strings.HasPrefix(text, "/setalert") {
				alertChatID = fmt.Sprintf("%d", chatID)
				response := fmt.Sprintf("✅ Alerts will now be sent to this chat!\n🆔 Chat ID: `%d`", chatID)
				sendTelegramMessage(alertChatID, response)
				fmt.Printf("\n[TELEGRAM] Alert destination changed to: %s\n", alertChatID)
			}

			// Handle /status command
			if strings.HasPrefix(text, "/status") {
				count := atomic.LoadUint64(&scanCount)
				knownHackathonMu.RLock()
				numKnown := len(knownHackathon)
				knownHackathonMu.RUnlock()

				response := fmt.Sprintf("📊 *Scanner Status*\n\n"+
					"🔄 Scans completed: %d\n"+
					"🏆 Known hackathon coins: %d\n"+
					"🌐 Proxies loaded: %d\n"+
					"📍 Alert destination: `%s`",
					count, numKnown, len(proxies), alertChatID)
				sendTelegramMessage(fmt.Sprintf("%d", chatID), response)
			}

			// Handle /help command
			if strings.HasPrefix(text, "/help") || strings.HasPrefix(text, "/start") {
				response := `🔍 *Pump.fun Hackathon Scanner*

*Commands:*
/chatid - Get this chat's ID
/setalert - Set alerts to this chat
/status - Scanner status
/help - Show this message

When a hackathon winner is detected, you'll get an instant alert! 🚨`
				sendTelegramMessage(fmt.Sprintf("%d", chatID), response)
			}
		}
	}
}

func sendTelegramMessage(chatID, text string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TelegramBotToken)

	data := url.Values{}
	data.Set("chat_id", chatID)
	data.Set("text", text)
	data.Set("parse_mode", "Markdown")

	resp, err := http.PostForm(apiURL, data)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func sendTelegramAlert(coin Coin) {
	message := fmt.Sprintf(`🚨🏆 *HACKATHON WINNER DETECTED!* 🏆🚨

*%s* (%s)

💰 Market Cap: $%s
📍 Mint: `+"`%s`"+`

🔗 [View on Pump.fun](https://pump.fun/coin/%s)
🐦 Twitter: %s
🌐 Website: %s`,
		escapeMarkdown(coin.Name),
		escapeMarkdown(coin.Symbol),
		formatNumber(coin.USDMarketCap),
		coin.Mint,
		coin.Mint,
		orDefault(coin.Twitter, "N/A"),
		orDefault(coin.Website, "N/A"))

	if err := sendTelegramMessage(alertChatID, message); err != nil {
		fmt.Printf("\n[ERROR] Telegram send failed: %v\n", err)
	} else {
		fmt.Println("\n[TELEGRAM] Alert sent!")
	}
}

func escapeMarkdown(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"]", "\\]",
		"`", "\\`",
	)
	return replacer.Replace(s)
}

func formatNumber(n float64) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.2fM", n/1000000)
	} else if n >= 1000 {
		return fmt.Sprintf("%.2fK", n/1000)
	}
	return fmt.Sprintf("%.2f", n)
}

func fetchCoinsPage(offset int, wg *sync.WaitGroup, results chan<- []Coin) {
	defer wg.Done()

	var client *http.Client

	proxy := getNextProxy()
	if proxy != "" {
		proxyURL, _ := url.Parse(fmt.Sprintf("http://%s", proxy))
		client = &http.Client{
			Timeout: RequestTimeout,
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			},
		}
	} else {
		client = &http.Client{Timeout: RequestTimeout}
	}

	apiURL := fmt.Sprintf("https://frontend-api-v3.pump.fun/coins?offset=%d&limit=%d&sort=market_cap&includeNsfw=false&order=DESC", offset, CoinsPerRequest)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		results <- nil
		return
	}

	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://pump.fun")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Sec-Ch-Ua", `"Not(A:Brand";v="8", "Chromium";v="144", "Google Chrome";v="144"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-site")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		// Fallback to direct if proxy fails
		if proxy != "" {
			client = &http.Client{Timeout: RequestTimeout}
			resp, err = client.Do(req)
			if err != nil {
				results <- nil
				return
			}
		} else {
			results <- nil
			return
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		results <- nil
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		results <- nil
		return
	}

	var coins []Coin
	if err := json.Unmarshal(body, &coins); err != nil {
		results <- nil
		return
	}

	results <- coins
}

func scanAllCoins() {
	start := time.Now()
	atomic.AddUint64(&scanCount, 1)
	count := atomic.LoadUint64(&scanCount)

	numRequests := TotalCoins / CoinsPerRequest
	results := make(chan []Coin, numRequests)
	var wg sync.WaitGroup

	// Launch all requests in parallel
	for offset := 0; offset < TotalCoins; offset += CoinsPerRequest {
		wg.Add(1)
		go fetchCoinsPage(offset, &wg, results)
	}

	// Wait and close
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	var allCoins []Coin
	for coins := range results {
		if coins != nil {
			allCoins = append(allCoins, coins...)
		}
	}

	elapsed := time.Since(start)

	// Find hackathon coins
	var hackathonCoins []Coin
	for _, coin := range allCoins {
		if coin.IsHackathon {
			hackathonCoins = append(hackathonCoins, coin)
		}
	}

	// Find NEW hackathon coins
	var newHackathonCoins []Coin
	knownHackathonMu.RLock()
	for _, coin := range hackathonCoins {
		if !knownHackathon[coin.Mint] {
			newHackathonCoins = append(newHackathonCoins, coin)
		}
	}
	knownHackathonMu.RUnlock()

	// Status line
	timestamp := time.Now().Format("15:04:05")
	fmt.Printf("\r[%s] #%d | %d coins | %dms | Hackathon: %d     ",
		timestamp, count, len(allCoins), elapsed.Milliseconds(), len(hackathonCoins))

	// Alert on new hackathon coins
	if len(newHackathonCoins) > 0 && !isFirstScan {
		fmt.Println()
		fmt.Println(strings.Repeat("═", 60))
		fmt.Println("🚨🏆 NEW HACKATHON WINNER DETECTED! 🏆🚨")
		fmt.Println(strings.Repeat("═", 60))

		for _, coin := range newHackathonCoins {
			fmt.Printf(`
  🏆 %s (%s)
  💰 Market Cap: $%s
  📍 Mint: %s
  🔗 https://pump.fun/coin/%s
  🐦 %s
  🌐 %s
`,
				coin.Name, coin.Symbol,
				formatNumber(coin.USDMarketCap),
				coin.Mint,
				coin.Mint,
				orDefault(coin.Twitter, "No Twitter"),
				orDefault(coin.Website, "No Website"))

			// Send Telegram alert
			sendTelegramAlert(coin)

			knownHackathonMu.Lock()
			knownHackathon[coin.Mint] = true
			knownHackathonMu.Unlock()
		}

		fmt.Println(strings.Repeat("═", 60))

		// Sound alert (macOS only)
		if runtime.GOOS == "darwin" {
			exec.Command("afplay", "/System/Library/Sounds/Glass.aiff").Start()
		}
	}

	// First scan - populate known
	if isFirstScan {
		knownHackathonMu.Lock()
		for _, coin := range hackathonCoins {
			knownHackathon[coin.Mint] = true
		}
		knownHackathonMu.Unlock()

		if len(hackathonCoins) > 0 {
			fmt.Printf("\n[INIT] Found %d existing hackathon coins\n", len(hackathonCoins))
		} else {
			fmt.Println("\n[INIT] No existing hackathon coins")
		}
		isFirstScan = false
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func main() {
	alertChatID = TelegramChatID

	fmt.Println("🔍 Pump.fun Hackathon Scanner")
	fmt.Printf("   Monitoring top %d coins\n", TotalCoins)
	fmt.Printf("   Poll interval: %s\n", PollInterval)
	fmt.Printf("   Workers: %d goroutines\n", TotalCoins/CoinsPerRequest)

	loadProxies()
	if len(proxies) > 0 {
		fmt.Printf("   Proxies: %d loaded\n", len(proxies))
	} else {
		fmt.Println("   Proxies: none (direct mode)")
	}

	fmt.Printf("   Telegram: alerts → %s\n", alertChatID)
	fmt.Println("   Bot Commands: /chatid /setalert /status /help")
	fmt.Println()

	// Start Telegram command handler in background
	go handleTelegramCommands()

	// Send startup notification
	sendTelegramMessage(alertChatID, "🔍 *Hackathon Scanner Started!*\n\nMonitoring pump.fun for hackathon winners...")

	// Initial scan
	scanAllCoins()

	// Continuous polling
	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for range ticker.C {
		scanAllCoins()
	}
}
