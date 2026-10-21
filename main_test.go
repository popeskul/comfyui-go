package main

import (
	"testing"
)

func TestConfigRequiresStrongToken(t *testing.T) {
	for _, token := range []string{"", "short"} {
		_, err := loadConfig(func(key string) string {
			if key == "API_TOKEN" {
				return token
			}

			return ""
		})
		if err == nil {
			t.Fatal("weak token accepted")
		}
	}
}

func TestConfigRejectsBadLimits(t *testing.T) {
	for _, field := range []string{"MAX_QUEUE", "MAX_JOBS", "MAX_RESULT_BYTES", "JOB_TIMEOUT"} {
		t.Run(field, func(t *testing.T) {
			_, err := loadConfig(func(key string) string {
				if key == "API_TOKEN" {
					return "12345678901234567890123456789012"
				}

				if key == field {
					return "-1"
				}

				return ""
			})
			if err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestConfigDefaults(t *testing.T) {
	c, err := loadConfig(func(key string) string {
		if key == "API_TOKEN" {
			return "12345678901234567890123456789012"
		}

		return ""
	})
	if err != nil || c.addr != "127.0.0.1:8088" || c.queue != 32 {
		t.Fatalf("%+v %v", c, err)
	}
}
