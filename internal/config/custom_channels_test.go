package config

import (
	"strings"
	"testing"
)

func TestNormalizeCustomChannels(t *testing.T) {
	c := Default()
	c.CustomChannels = []CustomChannel{
		{Name: "  a  ", BaseURL: "http://127.0.0.1:8800/v1/", APIKey: " sk-a ", Models: []string{" m1 ", "", "m2"}},
		{Name: "b", BaseURL: "https://example.com/v1", APIKey: "", Models: []string{"m3"}, Rate: 2.5},
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(c.CustomChannels) != 2 {
		t.Fatalf("应有 2 个实例，实际 %d", len(c.CustomChannels))
	}
	a := c.CustomChannels[0]
	if a.Name != "a" || a.BaseURL != "http://127.0.0.1:8800/v1" || a.APIKey != "sk-a" {
		t.Errorf("清洗错: %+v", a)
	}
	if len(a.Models) != 2 || a.Models[0] != "m1" || a.Models[1] != "m2" {
		t.Errorf("模型去空错: %+v", a.Models)
	}
}

func TestNormalizeCustomChannelsRejectsDuplicateName(t *testing.T) {
	c := Default()
	c.CustomChannels = []CustomChannel{
		{Name: "dup", BaseURL: "http://a/v1"},
		{Name: "dup", BaseURL: "http://b/v1"},
	}
	if err := c.normalize(); err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复名应报错，实际 %v", err)
	}
}

func TestNormalizeCustomChannelsRejectsBadURL(t *testing.T) {
	c := Default()
	c.CustomChannels = []CustomChannel{{Name: "x", BaseURL: "ftp://host/v1"}}
	if err := c.normalize(); err == nil || !strings.Contains(err.Error(), "协议") {
		t.Fatalf("ftp 应报错，实际 %v", err)
	}
	c = Default()
	c.CustomChannels = []CustomChannel{{Name: "x", BaseURL: "not a url"}}
	if err := c.normalize(); err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("非法 URL 应报错，实际 %v", err)
	}
}

func TestDefaultHasEmptyCustomChannels(t *testing.T) {
	c := Default()
	if c.CustomChannels == nil || len(c.CustomChannels) != 0 {
		t.Fatalf("默认应为空切片（非 nil），实际 %+v", c.CustomChannels)
	}
}

// TestCustomChannelsMaxConcurrency 并发上限归一：0=不限、负数归 0、超 32 钳到 32。
func TestCustomChannelsMaxConcurrency(t *testing.T) {
	c := Default()
	c.CustomChannels = []CustomChannel{
		{Name: "a", BaseURL: "http://h:1/v1", MaxConcurrency: 0},
		{Name: "b", BaseURL: "http://h:2/v1", MaxConcurrency: -3},
		{Name: "c", BaseURL: "http://h:3/v1", MaxConcurrency: 2},
		{Name: "d", BaseURL: "http://h:4/v1", MaxConcurrency: 999},
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	got := map[string]int{}
	for _, cc := range c.CustomChannels {
		got[cc.Name] = cc.MaxConcurrency
	}
	want := map[string]int{"a": 0, "b": 0, "c": 2, "d": 32}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s 的 max_concurrency 期望 %d，实际 %d", k, v, got[k])
		}
	}
}
