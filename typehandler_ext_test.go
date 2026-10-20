package higomapper

import (
	"testing"
)

func TestJsonHandlerRoundTrip(t *testing.T) {
	h := JsonHandler{}

	// 读取：合法 JSON 文本
	v, err := h.GetResult(`{"a":1,"b":[true,null],"c":"x"}`)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	m, ok := v.(map[string]any)
	if !ok || m["a"] != int64(1) {
		t.Fatalf("读取结果 = %#v", v)
	}
	arr := m["b"].([]any)
	if arr[0] != true || arr[1] != nil {
		t.Fatalf("嵌套数组 = %#v", arr)
	}

	// "null" 文本 → nil
	if v, err = h.GetResult("null"); err != nil || v != nil {
		t.Fatalf("null 文本: %v %v", v, err)
	}

	// []byte 同 string
	if v, err = h.GetResult([]byte(`[1,2]`)); err != nil {
		t.Fatalf("[]byte: %v", err)
	} else if s := v.([]any); len(s) != 2 || s[0] != int64(1) {
		t.Fatalf("[]byte 结果 = %#v", v)
	}

	// 非法 JSON 报错
	for _, bad := range []string{`{`, `{"a":}`, `1 2`, ``} {
		if _, err := h.GetResult(bad); err == nil {
			t.Errorf("非法 JSON %q 应报错", bad)
		}
	}

	// 绑定：字符串透传
	s, err := h.SetParameter(`{"k":true}`)
	if err != nil || s != `{"k":true}` {
		t.Fatalf("字符串透传: %v %v", s, err)
	}
	// 绑定：map 序列化
	s, err = h.SetParameter(map[string]any{"k": true})
	if err != nil || s != `{"k":true}` {
		t.Fatalf("map 序列化: %v %v", s, err)
	}
	// 绑定：null
	if s, err = h.SetParameter(nil); err != nil || s != nil {
		t.Fatalf("null 绑定: %v %v", s, err)
	}
	// 绑定：数字保精度（int64 → 无浮点转换）
	s, err = h.SetParameter(int64(9007199254740993))
	if err != nil || s != "9007199254740993" {
		t.Fatalf("大整数精度: %v %v", s, err)
	}
}

func TestDecimalHandler(t *testing.T) {
	h := DecimalHandler{}

	// 读取：文本严格校验
	for _, c := range []struct{ in, want string }{
		{"12.34", "12.34"}, {"-0.5", "-0.5"}, {"+3", "+3"},
		{"100", "100"}, {".5", ".5"}, {"5.", "5."},
	} {
		v, err := h.GetResult(c.in)
		if err != nil {
			t.Errorf("读取 %q: %v", c.in, err)
			continue
		}
		if v != c.want {
			t.Errorf("读取 %q = %v, want %v", c.in, v, c.want)
		}
	}
	for _, bad := range []string{"", "abc", "1.2.3", "1e5", " 1", "1 ", "0x10"} {
		if _, err := h.GetResult(bad); err == nil {
			t.Errorf("读取 %q 应报错", bad)
		}
	}

	// 读取：int64 / float64 转文本
	if v, _ := h.GetResult(int64(-42)); v != "-42" {
		t.Fatalf("int64 读取 = %v", v)
	}
	if v, _ := h.GetResult(float64(0.1)); v != "0.1" {
		t.Fatalf("float64 读取 = %v", v)
	}
	if v, _ := h.GetResult(nil); v != nil {
		t.Fatalf("null 读取 = %v", v)
	}

	// 绑定：字符串严格校验
	if s, err := h.SetParameter("99.99"); err != nil || s != "99.99" {
		t.Fatalf("绑定文本: %v %v", s, err)
	}
	if _, err := h.SetParameter("1.2.3"); err == nil {
		t.Fatal("绑定非法文本应报错")
	}
	// 绑定：数字转换
	if s, err := h.SetParameter(int64(7)); err != nil || s != "7" {
		t.Fatalf("绑定 int64: %v %v", s, err)
	}
	if s, err := h.SetParameter(float64(12.5)); err != nil || s != "12.5" {
		t.Fatalf("绑定 float64: %v %v", s, err)
	}
	if s, err := h.SetParameter(nil); err != nil || s != nil {
		t.Fatalf("绑定 null: %v %v", s, err)
	}
	if _, err := h.SetParameter(true); err == nil {
		t.Fatal("绑定 bool 应报错")
	}
}

func TestTypeHandlerRegistryJsonDecimalRegistered(t *testing.T) {
	r := NewTypeHandlerRegistry()
	if h, ok := r.Lookup("json"); !ok || h.TypeName() != "json" {
		t.Fatal("json 处理器应默认注册")
	}
	// decimal 不再别名到 f64：必须是 DecimalHandler 本尊
	h, ok := r.Lookup("decimal")
	if !ok {
		t.Fatal("decimal 处理器应默认注册")
	}
	if _, isDecimal := h.(DecimalHandler); !isDecimal {
		t.Fatalf("decimal 查找应命中 DecimalHandler, got %T", h)
	}
}
