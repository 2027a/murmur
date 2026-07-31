package logging

import (
	"log"
	"sync/atomic"
)

const (
	debugLevel int32 = iota
	infoLevel
	warnLevel
	errorLevel
)

var configuredLevel atomic.Int32

func Configure(level string) {
	value := infoLevel
	switch level {
	case "debug":
		value = debugLevel
	case "warn":
		value = warnLevel
	case "error":
		value = errorLevel
	}
	configuredLevel.Store(value)
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

func Warnf(format string, args ...any) {
	if configuredLevel.Load() <= warnLevel {
		log.Printf("warn: "+format, args...)
	}
}
