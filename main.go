package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cinar/indicator"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
)

// ==========================================
// 1. STRUKTUR DATA & KONFIGURASI
// ==========================================

type Config struct {
	ApiKey     string    `json:"-"`
	SecretKey  string    `json:"-"`
	Passphrase string    `json:"-"`
	MarginUSDT float64   `json:"margin"`
	Leverage   int       `json:"leverage"`
	Coins      [3]string `json:"coins"`
	Mode       string    `json:"mode"`
	IsRunning  bool      `json:"isRunning"`
	Timeframe  string    `json:"timeframe"`
}

type CoinState struct {
	Symbol       string  `json:"symbol"`
	Price        float64 `json:"price"`
	Position     string  `json:"position"`
	EntryPrice   float64 `json:"entryPrice"`
	PnL          float64 `json:"pnl"`
	Contracts    int     `json:"contracts"`
	MinMarginReq float64 `json:"minMarginReq"`
	IsValidSize  bool    `json:"isValidSize"`
	Signal       string  `json:"signal"`
}

type AppState struct {
	Config Config
	Coins  map[string]*CoinState
	Logs   []string
	mu     sync.RWMutex
}

func loadConfigFromEnv() Config {
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("could not load .env: %v", err)
	}

	mode := strings.ToLower(strings.TrimSpace(os.Getenv("OKX_MODE")))
	if mode == "" {
		mode = "demo"
	}

	apiKeyEnv, secretKeyEnv, passphraseEnv := "OKX_DEMO_API_KEY", "OKX_DEMO_SECRET_KEY", "OKX_DEMO_PASSPHRASE"
	if mode == "live" {
		apiKeyEnv, secretKeyEnv, passphraseEnv = "OKX_API_KEY", "OKX_SECRET_KEY", "OKX_PASSPHRASE"
	}

	return Config{
		ApiKey:     os.Getenv(apiKeyEnv),
		SecretKey:  os.Getenv(secretKeyEnv),
		Passphrase: os.Getenv(passphraseEnv),
		Mode:       mode,
		MarginUSDT: 1.0,
		Leverage:   10,
		Coins:      [3]string{"DOGE-USDT-SWAP", "PEPE-USDT-SWAP", "ARB-USDT-SWAP"},
		IsRunning:  false,
		Timeframe:  "5m",
	}
}

var state = &AppState{
	Config: loadConfigFromEnv(),
	Coins:  make(map[string]*CoinState),
	Logs:   []string{"[SYSTEM] Bot initialized."},
}

var contractMultipliers = map[string]float64{
	"DOGE-USDT-SWAP": 10.0,
	"PEPE-USDT-SWAP": 1000000.0,
	"ARB-USDT-SWAP":  1.0,
	"WIF-USDT-SWAP":  1.0,
	"SOL-USDT-SWAP":  0.1,
}

// ==========================================
// 2. OKX API & SIGNATURE ENGINE
// ==========================================

func generateSignature(timestamp, method, requestPath, body, secretKey string) string {
	message := timestamp + method + requestPath + body
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func okxRequest(method, requestPath, body string) (map[string]interface{}, error) {
	state.mu.RLock()
	cfg := state.Config
	state.mu.RUnlock()
	if cfg.ApiKey == "" || cfg.SecretKey == "" || cfg.Passphrase == "" {
		return nil, fmt.Errorf("OKX %s credentials are not configured", cfg.Mode)
	}

	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	sign := generateSignature(timestamp, method, requestPath, body, cfg.SecretKey)

	req, err := http.NewRequest(method, "https://www.okx.com"+requestPath, bytes.NewBufferString(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("OK-ACCESS-KEY", cfg.ApiKey)
	req.Header.Set("OK-ACCESS-SIGN", sign)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", cfg.Passphrase)
	req.Header.Set("Content-Type", "application/json")
	if cfg.Mode == "demo" {
		req.Header.Set("x-simulated-trading", "1")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	if code, ok := result["code"].(string); ok && code != "0" {
		return nil, fmt.Errorf("OKX API Error: %s - %v", code, result["msg"])
	}

	return result, nil
}

func placeOrder(symbol, side string, size int) error {
	state.mu.RLock()
	leverage := state.Config.Leverage
	state.mu.RUnlock()
	payload := map[string]interface{}{
		"instId":  symbol,
		"tdMode":  "isolated",                  // Wajib untuk Futures
		"side":    side,                        // "buy" atau "sell"
		"ordType": "market",                    // Market order untuk eksekusi instan
		"sz":      fmt.Sprintf("%d", size),     // OKX mewajibkan sz bertipe string
		"lever":   fmt.Sprintf("%d", leverage), // Wajib ada karena tdMode = isolated
	}
	bodyBytes, _ := json.Marshal(payload)

	_, err := okxRequest("POST", "/api/v5/trade/order", string(bodyBytes))
	return err
}

// PERBAIKAN: Menambahkan parameter 'size' agar menutup posisi secara utuh, bukan hardcoded "1"
func closePosition(symbol, side string, size int) error {
	payload := map[string]interface{}{
		"instId":  symbol,
		"tdMode":  "isolated",
		"side":    side, // "sell" untuk menutup Long, "buy" untuk menutup Short
		"ordType": "market",
		"sz":      fmt.Sprintf("%d", size), // Menggunakan ukuran kontrak yang sebenarnya
	}
	bodyBytes, _ := json.Marshal(payload)

	_, err := okxRequest("POST", "/api/v5/trade/order", string(bodyBytes))
	return err
}

// ==========================================
// 3. WEBSOCKET & INDICATOR ENGINE
// ==========================================

var wsConnections = make(map[string]*websocket.Conn)
var candleData = make(map[string][]float64)

func startWebSocket(symbol string) {
	state.mu.RLock()
	mode := state.Config.Mode
	state.mu.RUnlock()
	url := "wss://ws.okx.com:8443/ws/v5/business"
	if mode == "demo" {
		url = "wss://wspap.okx.com:8443/ws/v5/business?brokerId=9999"
	}

	header := make(http.Header)
	header.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

	c, resp, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		addLog(fmt.Sprintf("[WS DIAL ERROR] %s: %v", symbol, err))
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(3 * time.Second)
		go startWebSocket(symbol)
		return
	}

	state.mu.Lock()
	wsConnections[symbol] = c
	state.mu.Unlock()

	state.mu.RLock()
	timeframe := state.Config.Timeframe
	state.mu.RUnlock()
	subMsg := map[string]interface{}{
		"op": "subscribe",
		"args": []map[string]string{
			{"channel": "candle" + timeframe, "instId": symbol},
		},
	}
	msgBytes, _ := json.Marshal(subMsg)

	err = c.WriteMessage(websocket.TextMessage, msgBytes)
	if err != nil {
		addLog(fmt.Sprintf("[WS SUBSCRIBE ERROR] %s: %v", symbol, err))
		c.Close()
		time.Sleep(3 * time.Second)
		go startWebSocket(symbol)
		return
	}

	addLog(fmt.Sprintf("[WS CONNECTED] %s berhasil terhubung.", symbol))

	go func() {
		done := make(chan struct{})
		var writeMu sync.Mutex
		go func() {
			ticker := time.NewTicker(20 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					writeMu.Lock()
					err := c.WriteMessage(websocket.TextMessage, []byte("ping"))
					writeMu.Unlock()
					if err != nil {
						c.Close()
						return
					}
				}
			}
		}()
		defer close(done)

		for {
			_, message, err := c.ReadMessage()
			if err != nil {
				addLog(fmt.Sprintf("[WS READ ERROR] %s: %v. Reconnecting...", symbol, err))
				c.Close()
				time.Sleep(3 * time.Second)
				go startWebSocket(symbol)
				return
			}

			msgStr := string(message)
			if msgStr == "ping" || msgStr == `{"event":"ping"}` {
				writeMu.Lock()
				err := c.WriteMessage(websocket.TextMessage, []byte("pong"))
				writeMu.Unlock()
				if err != nil {
					c.Close()
				}
				continue
			}

			var wsResp map[string]interface{}
			if err := json.Unmarshal(message, &wsResp); err != nil {
				continue
			}
			if event, ok := wsResp["event"].(string); ok {
				switch event {
				case "subscribe":
					addLog(fmt.Sprintf("[WS SUBSCRIBED] %s candle%s", symbol, timeframe))
				case "error":
					addLog(fmt.Sprintf("[WS SUBSCRIBE ERROR] %s: %v", symbol, wsResp["msg"]))
				}
				continue
			}

			if data, ok := wsResp["data"].([]interface{}); ok && len(data) > 0 {
				if candleArr, ok := data[0].([]interface{}); ok && len(candleArr) >= 5 {
					closePriceStr, ok := candleArr[4].(string)
					if !ok {
						addLog(fmt.Sprintf("[WS DATA ERROR] %s: candle close is not a string", symbol))
						continue
					}
					closePrice, err := strconv.ParseFloat(closePriceStr, 64)
					if err != nil || closePrice <= 0 {
						addLog(fmt.Sprintf("[WS DATA ERROR] %s: invalid close price %q", symbol, closePriceStr))
						continue
					}

					state.mu.Lock()
					if state.Coins[symbol] == nil {
						state.Coins[symbol] = &CoinState{Symbol: symbol, Position: "NONE"}
					}
					firstPrice := state.Coins[symbol].Price == 0
					state.Coins[symbol].Price = closePrice

					candleData[symbol] = append(candleData[symbol], closePrice)
					if len(candleData[symbol]) > 100 {
						candleData[symbol] = candleData[symbol][1:]
					}

					isRunning := state.Config.IsRunning
					state.mu.Unlock()
					if firstPrice {
						addLog(fmt.Sprintf("[WS PRICE] %s: %.8f", symbol, closePrice))
					}

					if isRunning {
						checkTradingSignal(symbol, closePrice)
					}
				}
			}
		}
	}()
}

func checkTradingSignal(symbol string, currentPrice float64) {
	state.mu.RLock()
	cs := state.Coins[symbol]
	if cs == nil {
		state.mu.RUnlock()
		return
	}
	prices := append([]float64(nil), candleData[symbol]...)
	position, contracts, isValidSize, entryPrice := cs.Position, cs.Contracts, cs.IsValidSize, cs.EntryPrice
	state.mu.RUnlock()

	if len(prices) < 20 {
		return
	}

	_, upperBand, lowerBand := indicator.BollingerBands(prices)

	rsi, _ := indicator.Rsi(prices)
	currentRSI := rsi[len(rsi)-1]

	if position == "NONE" && currentPrice <= lowerBand[len(lowerBand)-1]*1.001 && currentRSI < 30 {
		state.mu.Lock()
		cs.Signal = "BUY SIGNAL (BB Lower + RSI<30)"
		state.mu.Unlock()
		addLog(fmt.Sprintf("[SIGNAL] %s: BUY | Price: %.6f | RSI: %.2f", symbol, currentPrice, currentRSI))

		if isValidSize {
			err := placeOrder(symbol, "buy", contracts)
			if err == nil {
				state.mu.Lock()
				cs.Position = "LONG"
				cs.EntryPrice = currentPrice
				state.mu.Unlock()
				addLog(fmt.Sprintf("[EXECUTED] LONG %s | Size: %d contracts", symbol, contracts))
			} else {
				addLog(fmt.Sprintf("[ERROR] Gagal order %s: %v", symbol, err))
			}
		}
	}

	if position == "NONE" && currentPrice >= upperBand[len(upperBand)-1]*0.999 && currentRSI > 70 {
		state.mu.Lock()
		cs.Signal = "SELL SIGNAL (BB Upper + RSI>70)"
		state.mu.Unlock()
		addLog(fmt.Sprintf("[SIGNAL] %s: SELL | Price: %.6f | RSI: %.2f", symbol, currentPrice, currentRSI))

		if isValidSize {
			err := placeOrder(symbol, "sell", contracts)
			if err == nil {
				state.mu.Lock()
				cs.Position = "SHORT"
				cs.EntryPrice = currentPrice
				state.mu.Unlock()
				addLog(fmt.Sprintf("[EXECUTED] SHORT %s | Size: %d contracts", symbol, contracts))
			} else {
				addLog(fmt.Sprintf("[ERROR] Gagal order %s: %v", symbol, err))
			}
		}
	}

	switch position {
	case "LONG":
		// TP: 0.3% | SL: 0.5%
		if entryPrice > 0 && ((currentPrice-entryPrice)/entryPrice >= 0.003 || (entryPrice-currentPrice)/entryPrice >= 0.005) {
			err := closePosition(symbol, "sell", contracts) // PERBAIKAN: Gunakan cs.Contracts
			if err == nil {
				state.mu.Lock()
				cs.Position = "NONE"
				state.mu.Unlock()
				addLog(fmt.Sprintf("[EXIT] LONG %s closed | Size: %d contracts", symbol, contracts))
			} else {
				addLog(fmt.Sprintf("[ERROR] Gagal close LONG %s: %v", symbol, err))
			}
		}
	case "SHORT":
		if entryPrice > 0 && ((entryPrice-currentPrice)/entryPrice >= 0.003 || (currentPrice-entryPrice)/entryPrice >= 0.005) {
			err := closePosition(symbol, "buy", contracts) // PERBAIKAN: Gunakan cs.Contracts
			if err == nil {
				state.mu.Lock()
				cs.Position = "NONE"
				state.mu.Unlock()
				addLog(fmt.Sprintf("[EXIT] SHORT %s closed | Size: %d contracts", symbol, contracts))
			} else {
				addLog(fmt.Sprintf("[ERROR] Gagal close SHORT %s: %v", symbol, err))
			}
		}
	}
}

// ==========================================
// 4. UTILITIES & STATE MANAGEMENT
// ==========================================

func calculateOrderSize(symbol string, margin float64, leverage int) (int, float64, bool) {
	multiplier, exists := contractMultipliers[symbol]
	if !exists {
		return 0, 0, false
	}

	coin := state.Coins[symbol]
	if coin == nil || coin.Price == 0 || leverage <= 0 {
		return 0, 0, false
	}

	valuePerContract := coin.Price * multiplier
	targetNotional := margin * float64(leverage)
	contracts := int(math.Floor(targetNotional / valuePerContract))
	minMarginForOne := valuePerContract / float64(leverage)

	return contracts, minMarginForOne, contracts >= 1
}

func addLog(msg string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	timestamp := time.Now().Format("15:04:05")
	state.Logs = append([]string{fmt.Sprintf("[%s] %s", timestamp, msg)}, state.Logs...)
	if len(state.Logs) > 50 {
		state.Logs = state.Logs[:50]
	}
}

// ==========================================
// 5. HTTP HANDLERS
// ==========================================

func handleIndex(w http.ResponseWriter, r *http.Request) {
	tmpl, _ := template.New("web").Parse(uiTemplate)
	tmpl.Execute(w, nil)
}

func handleGetState(w http.ResponseWriter, r *http.Request) {
	state.mu.Lock()
	newSymbols := make([]string, 0)

	for _, sym := range state.Config.Coins {
		if _, exists := state.Coins[sym]; !exists {
			state.Coins[sym] = &CoinState{Symbol: sym, Position: "NONE"}
			newSymbols = append(newSymbols, sym)
		}
		contracts, minReq, isValid := calculateOrderSize(sym, state.Config.MarginUSDT, state.Config.Leverage)
		state.Coins[sym].Contracts = contracts
		state.Coins[sym].MinMarginReq = minReq
		state.Coins[sym].IsValidSize = isValid
	}

	response := struct {
		Config Config
		Coins  map[string]*CoinState
		Logs   []string
	}{Config: state.Config, Coins: make(map[string]*CoinState, len(state.Coins)), Logs: append([]string(nil), state.Logs...)}
	for symbol, coin := range state.Coins {
		coinCopy := *coin
		response.Coins[symbol] = &coinCopy
	}
	state.mu.Unlock()

	for _, sym := range newSymbols {
		go startWebSocket(sym)
	}
	json.NewEncoder(w).Encode(response)
}

func handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var newConfig Config
	if err := json.NewDecoder(r.Body).Decode(&newConfig); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	state.mu.Lock()
	// Demo API credentials are loaded from the environment and are not updated through the UI.
	state.Config.MarginUSDT = newConfig.MarginUSDT
	state.Config.Leverage = newConfig.Leverage
	state.Config.Coins = newConfig.Coins
	state.Config.IsRunning = newConfig.IsRunning
	state.Config.Timeframe = newConfig.Timeframe
	coins := state.Config.Coins
	state.mu.Unlock()

	if newConfig.IsRunning {
		addLog("CONFIG: Bot STARTED. Menghubungkan ke OKX WebSocket...")
		for _, sym := range coins {
			state.mu.RLock()
			if _, exists := wsConnections[sym]; !exists {
				state.mu.RUnlock()
				go startWebSocket(sym)
			} else {
				state.mu.RUnlock()
			}
		}
	} else {
		addLog("CONFIG: Bot STOPPED.")
	}
	w.WriteHeader(200)
}

func handleEmergencyClose(w http.ResponseWriter, r *http.Request) {
	type positionToClose struct {
		symbol    string
		position  string
		contracts int
	}
	state.mu.Lock()
	positions := make([]positionToClose, 0)
	for symbol, coin := range state.Coins {
		if coin.Position != "NONE" {
			positions = append(positions, positionToClose{symbol: symbol, position: coin.Position, contracts: coin.Contracts})
		}
	}
	state.Config.IsRunning = false
	state.mu.Unlock()

	failed := false
	for _, position := range positions {
		addLog(fmt.Sprintf("[EMERGENCY] Attempting to close %s %s", position.position, position.symbol))
		closeSide := "sell"
		if position.position == "SHORT" {
			closeSide = "buy"
		}
		if err := closePosition(position.symbol, closeSide, position.contracts); err != nil {
			failed = true
			addLog(fmt.Sprintf("[EMERGENCY ERROR] Gagal menutup %s: %v", position.symbol, err))
			continue
		}
		state.mu.Lock()
		if coin := state.Coins[position.symbol]; coin != nil {
			coin.Position = "NONE"
			coin.EntryPrice = 0
			coin.PnL = 0
		}
		state.mu.Unlock()
		addLog(fmt.Sprintf("[EMERGENCY SUCCESS] Berhasil menutup %s %s", position.symbol, position.position))
	}
	addLog("[EMERGENCY] Bot dihentikan secara paksa.")
	if failed {
		http.Error(w, "one or more positions could not be closed; check bot logs and OKX", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ==========================================
// 6. MAIN ENTRY POINT
// ==========================================

func main() {
	if state.Config.Mode != "demo" && state.Config.Mode != "live" {
		log.Fatalf("invalid OKX_MODE %q: use demo or live", state.Config.Mode)
	}
	if state.Config.Mode == "live" {
		log.Println("WARNING: LIVE TRADING MODE. Orders will use real funds.")
	} else {
		log.Println("OKX demo trading mode enabled.")
	}
	addLog(fmt.Sprintf("[SYSTEM] Trading mode: %s", strings.ToUpper(state.Config.Mode)))

	for _, sym := range state.Config.Coins {
		state.Coins[sym] = &CoinState{Symbol: sym, Position: "NONE"}
		candleData[sym] = make([]float64, 0)
		go startWebSocket(sym)
	}

	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/state", handleGetState)
	http.HandleFunc("/api/config", handleUpdateConfig)
	http.HandleFunc("/api/emergency", handleEmergencyClose)

	fmt.Println("========================================")
	fmt.Printf("  OKX SCALPER PRO (%s MODE)\n", strings.ToUpper(state.Config.Mode))
	fmt.Println("  UI Ready at: http://103.186.30.230:8080")
	fmt.Println("========================================")

	log.Fatal(http.ListenAndServe("0.0.0.0:8080", nil))
}

// ==========================================
// 7. UI TEMPLATE (API INPUTS REMOVED)
// ==========================================

const uiTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>OKX Algo-Trader Pro</title>
    <script src="https://cdn.tailwindcss.com"></script>
    <script defer src="https://unpkg.com/alpinejs@3.x.x/dist/cdn.min.js"></script>
    <link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@300;400;500;600;700;800&display=swap" rel="stylesheet">
    <style>
        body { font-family: 'Plus Jakarta Sans', sans-serif; background: #05070d; overflow-x: hidden; }
        .aurora-bg { position: fixed; top: 0; left: 0; width: 100%; height: 100%; z-index: -1; background: #05070d; overflow: hidden; }
        .aurora-bg::before { content: ''; position: absolute; top: -50%; left: -50%; width: 200%; height: 200%; background: radial-gradient(circle at 20% 30%, rgba(56, 189, 248, 0.15) 0%, transparent 40%), radial-gradient(circle at 80% 70%, rgba(168, 85, 247, 0.15) 0%, transparent 40%); animation: rotateAurora 30s linear infinite; }
        @keyframes rotateAurora { from { transform: rotate(0deg); } to { transform: rotate(360deg); } }
        .glass { background: rgba(255, 255, 255, 0.03); backdrop-filter: blur(20px); -webkit-backdrop-filter: blur(20px); border: 1px solid rgba(255, 255, 255, 0.08); box-shadow: 0 8px 32px 0 rgba(0, 0, 0, 0.3); }
        .glass-hover:hover { background: rgba(255, 255, 255, 0.05); border-color: rgba(255, 255, 255, 0.15); transform: translateY(-2px); transition: all 0.3s cubic-bezier(0.4, 0, 0.2, 1); }
        .neon-cyan { box-shadow: 0 0 15px rgba(34, 211, 238, 0.3); }
        .neon-rose { box-shadow: 0 0 15px rgba(244, 63, 94, 0.3); }
        .slider-pro { -webkit-appearance: none; appearance: none; width: 100%; height: 6px; border-radius: 5px; background: rgba(255, 255, 255, 0.1); outline: none; }
        .slider-pro::-webkit-slider-thumb { -webkit-appearance: none; width: 22px; height: 22px; border-radius: 50%; background: linear-gradient(135deg, #22d3ee, #a855f7); cursor: pointer; border: 2px solid rgba(255, 255, 255, 0.5); box-shadow: 0 0 15px rgba(34, 211, 238, 0.6); }
        ::-webkit-scrollbar { width: 6px; } ::-webkit-scrollbar-track { background: transparent; } ::-webkit-scrollbar-thumb { background: rgba(255, 255, 255, 0.1); border-radius: 10px; }
    </style>
</head>
<body class="text-white min-h-screen p-4 md:p-8" x-data="botApp()" x-init="init()">
    <div class="aurora-bg"></div>

    <header class="flex flex-col md:flex-row justify-between items-start md:items-center mb-8 gap-4">
        <div class="flex items-center gap-4">
            <div class="w-12 h-12 rounded-xl bg-gradient-to-br from-cyan-500 to-purple-600 flex items-center justify-center shadow-lg neon-cyan">
                <svg class="w-6 h-6 text-white" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M13 10V3L4 14h7v7l9-11h-7z"></path></svg>
            </div>
            <div>
                <h1 class="text-2xl font-bold bg-clip-text text-transparent bg-gradient-to-r from-cyan-400 to-purple-400">OKX SCALPER PRO</h1>
				<p class="text-xs text-white/50 font-medium tracking-wider uppercase" x-text="mode === 'demo' ? 'OKX Demo Trading • Simulated Orders' : 'OKX Live Trading • Real Orders'"></p>
            </div>
        </div>
        <div class="flex items-center gap-3">
            <div class="flex items-center gap-2 px-4 py-2 rounded-full glass" :class="isRunning ? 'border-emerald-500/30' : 'border-white/10'">
                <span class="relative flex h-2 w-2">
                    <span class="animate-ping absolute inline-flex h-full w-full rounded-full opacity-75" :class="isRunning ? 'bg-emerald-400' : 'bg-rose-400'"></span>
                    <span class="relative inline-flex rounded-full h-2 w-2" :class="isRunning ? 'bg-emerald-500' : 'bg-rose-500'"></span>
                </span>
                <span class="text-xs font-bold tracking-wider" :class="isRunning ? 'text-emerald-400' : 'text-white/60'" x-text="isRunning ? 'SYSTEM ONLINE' : 'SYSTEM OFFLINE'"></span>
            </div>
            <button @click="emergencyClose()" class="px-5 py-2.5 bg-rose-500/10 border border-rose-500/30 text-rose-400 text-xs font-bold rounded-xl hover:bg-rose-500/20 transition-all flex items-center gap-2">
                <svg class="w-4 h-4" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-3L13.732 4c-.77-1.333-2.694-1.333-3.464 0L3.34 16c-.77 1.333.192 3 1.732 3z"></path></svg>
                KILL SWITCH
            </button>
        </div>
    </header>

    <div class="grid grid-cols-1 lg:grid-cols-12 gap-6">
        <div class="lg:col-span-4 space-y-6">
            <div class="glass rounded-3xl p-6 glass-hover transition-all">
                <h2 class="text-lg font-bold text-white/90 mb-6 flex items-center gap-3">
                    <div class="w-8 h-8 rounded-lg bg-purple-500/20 flex items-center justify-center"><svg class="w-4 h-4 text-purple-400" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 6V4m0 2a2 2 0 100 4m0-4a2 2 0 110 4m-6 8a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4m6 6v10m6-2a2 2 0 100-4m0 4a2 2 0 110-4m0 4v2m0-6V4"></path></svg></div>
                    Trade Configuration
                </h2>
                <div class="space-y-5">
                    <div>
                        <label class="block text-xs text-white/50 mb-2 font-semibold uppercase tracking-wider">Margin per Trade (USDT)</label>
                        <input type="number" step="0.1" x-model.number="margin" @input="updateConfig()" class="w-full bg-black/20 border border-white/10 rounded-xl px-4 py-3 text-cyan-300 font-mono text-lg focus:outline-none focus:border-cyan-400/50">
                    </div>

                    <div>
                        <div class="flex justify-between items-center mb-2">
                            <label class="text-xs text-white/50 font-semibold uppercase tracking-wider">Leverage</label>
                            <span class="text-sm font-bold bg-clip-text text-transparent bg-gradient-to-r from-cyan-400 to-purple-400" x-text="leverage + 'x'"></span>
                        </div>
                        <input type="range" min="1" max="50" x-model.number="leverage" @input="updateConfig()" class="w-full slider-pro">
                    </div>

                    <div class="space-y-3">
                        <label class="block text-xs text-white/50 font-semibold uppercase tracking-wider">Instruments (Altcoins)</label>
                        <template x-for="(coin, index) in coins" :key="index">
                            <select x-model="coins[index]" @change="updateConfig()" class="w-full bg-black/20 border border-white/10 rounded-xl px-4 py-2.5 text-sm text-white/90 focus:outline-none focus:border-purple-400/50">
                                <option value="DOGE-USDT-SWAP" class="bg-slate-900">DOGE (Min ~$0.15)</option>
                                <option value="PEPE-USDT-SWAP" class="bg-slate-900">PEPE (Min ~$1.00)</option>
                                <option value="ARB-USDT-SWAP" class="bg-slate-900">ARB (Min ~$1.20)</option>
                                <option value="WIF-USDT-SWAP" class="bg-slate-900">WIF (Min ~$2.50)</option>
                            </select>
                        </template>
                    </div>

                    <button @click="toggleBot()" :class="isRunning ? 'bg-gradient-to-r from-rose-600 to-pink-600 neon-rose' : 'bg-gradient-to-r from-cyan-600 to-blue-600 neon-cyan'" class="w-full py-4 rounded-xl font-bold text-white shadow-lg transition-all transform active:scale-[0.98] flex items-center justify-center gap-2 mt-4">
                        <span x-text="isRunning ? 'STOP ENGINE' : 'START ENGINE'"></span>
                    </button>
                </div>
            </div>

            <div class="glass rounded-3xl p-6 glass-hover transition-all">
                <h2 class="text-sm font-bold text-white/90 mb-4 flex items-center gap-2">
                    <span class="relative flex h-2 w-2"><span class="animate-ping absolute inline-flex h-full w-full rounded-full bg-emerald-400 opacity-75"></span><span class="relative inline-flex rounded-full h-2 w-2 bg-emerald-500"></span></span>
                    LIVE LOGS
                </h2>
                <div class="bg-black/40 rounded-xl p-4 h-56 overflow-y-auto font-mono text-[11px] text-emerald-400/80 border border-white/5 shadow-inner">
                    <template x-for="(log, index) in logs" :key="index">
                        <div class="mb-1.5 break-all leading-relaxed border-l-2 border-emerald-500/30 pl-2" x-text="log"></div>
                    </template>
                </div>
            </div>
        </div>

        <div class="lg:col-span-8 grid grid-cols-1 md:grid-cols-3 gap-5">
            <template x-for="(coin, index) in coins" :key="index">
                <div class="glass rounded-3xl p-6 glass-hover transition-all flex flex-col relative overflow-hidden" :class="!coinStates[coin]?.isValidSize ? 'border-rose-500/50 neon-rose' : 'border-white/5'">
                    <div class="absolute -top-10 -right-10 w-32 h-32 rounded-full blur-3xl opacity-20" :class="{'bg-cyan-500': coinStates[coin]?.position === 'LONG', 'bg-rose-500': coinStates[coin]?.position === 'SHORT', 'bg-white': coinStates[coin]?.position === 'NONE'}"></div>
                    <div class="flex justify-between items-center mb-6 relative z-10">
                        <div class="flex items-center gap-3">
                            <div class="w-10 h-10 rounded-xl bg-white/5 flex items-center justify-center border border-white/10"><span class="text-lg font-bold" x-text="coin.split('-')[0].substring(0, 2)"></span></div>
                            <div><h3 class="text-lg font-bold text-white" x-text="coin.split('-')[0]"></h3><p class="text-[10px] text-white/40 font-medium">PERPETUAL</p></div>
                        </div>
                        <span class="px-3 py-1 text-[10px] font-bold rounded-lg backdrop-blur-sm border" :class="{'bg-cyan-500/20 text-cyan-300 border-cyan-500/30': coinStates[coin]?.position === 'LONG', 'bg-rose-500/20 text-rose-300 border-rose-500/30': coinStates[coin]?.position === 'SHORT', 'bg-white/5 text-white/40 border-white/10': coinStates[coin]?.position === 'NONE'}" x-text="coinStates[coin]?.position || 'IDLE'"></span>
                    </div>
                    <div class="mb-6 relative z-10">
                        <div class="text-3xl font-bold text-white tracking-tight font-mono" x-text="'$' + (coinStates[coin]?.price?.toFixed(6) || '0.000000')"></div>
                        <div class="text-[10px] text-purple-300 mt-1 font-mono" x-text="coinStates[coin]?.signal || ''"></div>
                        <div class="text-sm font-bold mt-2 flex items-center gap-1" :class="(coinStates[coin]?.pnl || 0) >= 0 ? 'text-emerald-400' : 'text-rose-400'">
                            <span x-text="(coinStates[coin]?.pnl || 0) >= 0 ? '+' : '' + '$' + (coinStates[coin]?.pnl?.toFixed(4) || '0.00')"></span>
                        </div>
                    </div>
                    <div class="space-y-3 text-xs text-white/50 border-t border-white/5 pt-4 mt-auto relative z-10">
                        <div class="flex justify-between"><span>Contracts:</span><span class="text-white/90 font-bold font-mono" x-text="coinStates[coin]?.contracts || 0"></span></div>
                        <div class="flex justify-between"><span>Entry Price:</span><span class="text-white/90 font-bold font-mono" x-text="coinStates[coin]?.entryPrice?.toFixed(6) || '-'"></span></div>
                        <template x-if="!coinStates[coin]?.isValidSize">
                            <div class="mt-3 p-3 bg-rose-500/10 border border-rose-500/30 rounded-xl text-[10px] text-rose-300 flex items-start gap-2">
                                <span>Margin $<span x-text="margin"></span> too small. Min req: $<span x-text="coinStates[coin]?.minMarginReq?.toFixed(2)"></span></span>
                            </div>
                        </template>
                    </div>
                </div>
            </template>
        </div>
    </div>

    <script>
        function botApp() {
            return {
				margin: 1.0, leverage: 10, mode: 'demo',
                coins: ['DOGE-USDT-SWAP', 'PEPE-USDT-SWAP', 'ARB-USDT-SWAP'],
                isRunning: false, coinStates: {}, logs: [],
                init() {
                    setInterval(async () => {
                        try {
                            const res = await fetch('/api/state');
                            const data = await res.json();
                            this.margin = data.Config.margin;
                            this.leverage = data.Config.leverage;
							this.mode = data.Config.mode;
                            this.coins = data.Config.coins;
                            this.isRunning = data.Config.isRunning;
                            this.coinStates = data.Coins;
                            this.logs = data.Logs;
                        } catch (e) { console.error(e); }
                    }, 500);
                },
                async updateConfig() {
                    await fetch('/api/config', {
                        method: 'POST', headers: {'Content-Type': 'application/json'},
                        body: JSON.stringify({ margin: this.margin, leverage: this.leverage, coins: this.coins, isRunning: this.isRunning, timeframe: '5m' })
                    });
                },
                async toggleBot() {
                    this.isRunning = !this.isRunning;
                    await this.updateConfig();
                },
                async emergencyClose() {
                    if(confirm('KILL SWITCH: Close ALL positions and stop bot?')) {
                        await fetch('/api/emergency', { method: 'POST' });
                        this.isRunning = false;
                    }
                }
            }
        }
    </script>
</body>
</html>
`
