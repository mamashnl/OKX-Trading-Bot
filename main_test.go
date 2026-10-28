package main

import "testing"

func TestADXValueDistinguishesTrendFromRange(t *testing.T) {
	trending := make([]CandleOHLC, 40)
	ranging := make([]CandleOHLC, 40)
	for i := range trending {
		open := 100 + float64(i)
		trending[i] = CandleOHLC{
			Open: open, High: open + 1, Low: open - 0.2, Close: open + 0.8,
		}
		ranging[i] = CandleOHLC{Open: 100, High: 101, Low: 99, Close: 100}
	}

	if !isTrendStrong(trending) {
		t.Fatal("expected a steadily rising market to pass the ADX trend filter")
	}
	if isTrendStrong(ranging) {
		t.Fatal("expected a flat market to fail the ADX trend filter")
	}
}

func TestAdvancedSMCRequiresVolumeSpikeAndReversal(t *testing.T) {
	candles := make([]CandleOHLC, 30)
	for i := range candles {
		candles[i] = CandleOHLC{Open: 100, High: 101, Low: 99, Close: 100, Volume: 10}
	}
	candles[len(candles)-1] = CandleOHLC{
		Open: 98.81, High: 99.2, Low: 98.8, Close: 98.9, Volume: 100,
	}

	if got := checkAdvancedSmc(candles, styleConservative); got != "LONG" {
		t.Fatalf("checkAdvancedSmc() = %q, want LONG", got)
	}

	candles[len(candles)-1].Volume = 10
	if got := checkAdvancedSmc(candles, styleAggressive); got != "NONE" {
		t.Fatalf("checkAdvancedSmc() without a volume spike = %q, want NONE", got)
	}

	candles[len(candles)-1] = CandleOHLC{
		Open: 101.19, High: 101.2, Low: 100.9, Close: 101.1, Volume: 100,
	}
	if got := checkAdvancedSmc(candles, styleConservative); got != "SHORT" {
		t.Fatalf("checkAdvancedSmc() = %q, want SHORT", got)
	}
}

func TestConfiguredSignalEvaluatesEachClosedCandleOnce(t *testing.T) {
	candles := make([]CandleOHLC, 222)
	for i := range candles {
		candles[i] = CandleOHLC{
			Ts: int64(i + 1), Open: 100, High: 101, Low: 99, Close: 100,
		}
	}
	cfg := Config{StrategyMode: stratEmaRsi, TradingStyle: styleAggressive}

	first := evaluateConfiguredSignal("TEST-USDT-SWAP", candles, 100, 220, cfg)
	if !first.Evaluated {
		t.Fatal("expected the newly closed candle to be evaluated")
	}
	if first.Triggered {
		t.Fatal("flat candles must not trigger an EMA/RSI entry")
	}

	repeated := evaluateConfiguredSignal("TEST-USDT-SWAP", candles, 100, candles[len(candles)-2].Ts, cfg)
	if repeated.Evaluated {
		t.Fatal("the same closed candle must not be evaluated twice")
	}
}
