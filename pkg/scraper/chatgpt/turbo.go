package chatgpt

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// turbo-stream 的特殊常量(见 jacob-ebey/turbo-stream/utils.js)。
const (
	turboHole            = -1
	turboNaN             = -2
	turboNegInf          = -3
	turboNegZero         = -4
	turboNull            = -5
	turboPosInf          = -6
	turboUndefined       = -7
	turboTypeBigInt      = "B"
	turboTypeDate        = "D"
	turboTypeError       = "E"
	turboTypeMap         = "M"
	turboTypeNullObject  = "N"
	turboTypePromise     = "P"
	turboTypeRegExp      = "R"
	turboTypeSet         = "S"
	turboTypeURL         = "U"
	turboTypeSymbol      = "Y"
	turboTypePrevResolve = "Z"
)

// decodeTurboStream 解码 React Router 的 turbo-stream 数据。
//
// 数据是一行 JSON 数组(值表),数组里存的是"引用下标";对象形如
// {"_1":2} 表示"键名在值表 1 号位置,值在值表 2 号位置"。
func decodeTurboStream(stream string) (any, error) {
	line := stream
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil, fmt.Errorf("空的流数据")
	}
	var values []any
	if err := json.Unmarshal([]byte(line), &values); err != nil {
		return nil, fmt.Errorf("解析流数据失败: %w", err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("流数据为空")
	}
	d := &turboDecoder{values: values, memo: map[int]any{}}
	return d.hydrate(0), nil
}

type turboDecoder struct {
	values []any
	memo   map[int]any
}

// hydrate 解析值表里第 index 项。
func (d *turboDecoder) hydrate(index int) any {
	switch index {
	case turboHole, turboUndefined, turboNull:
		return nil
	case turboNaN, turboNegInf, turboPosInf:
		return nil
	case turboNegZero:
		return 0
	}
	if v, ok := d.memo[index]; ok {
		return v
	}
	if index < 0 || index >= len(d.values) {
		return nil
	}
	raw := d.values[index]
	if n, ok := raw.(float64); ok {
		switch int(n) {
		case turboHole, turboUndefined, turboNull, turboNaN, turboNegInf, turboPosInf:
			d.memo[index] = nil
			return nil
		case turboNegZero:
			d.memo[index] = 0
			return 0
		}
		d.memo[index] = n
		return n
	}

	switch v := raw.(type) {
	case []any:
		if len(v) > 0 {
			if typ, ok := v[0].(string); ok {
				return d.hydrateTyped(index, typ, v)
			}
		}
		arr := make([]any, len(v))
		d.memo[index] = arr
		for i, item := range v {
			if isRefHole(item) {
				continue
			}
			arr[i] = d.resolve(item)
		}
		return arr
	case map[string]any:
		obj := make(map[string]any, len(v))
		d.memo[index] = obj
		for key, val := range v {
			obj[d.resolveKey(key)] = d.resolve(val)
		}
		return obj
	default:
		d.memo[index] = raw
		return raw
	}
}

// hydrateTyped 处理 ["D", ...] / ["P", ...] 这类带类型标记的值。
func (d *turboDecoder) hydrateTyped(index int, typ string, value []any) any {
	arg := func(i int) any {
		if i < len(value) {
			return d.resolve(value[i])
		}
		return nil
	}
	switch typ {
	case turboTypeDate, turboTypeURL, turboTypeBigInt:
		out := arg(1)
		d.memo[index] = out
		return out
	case turboTypeNullObject:
		obj := map[string]any{}
		d.memo[index] = obj
		if raw, ok := value[1].(map[string]any); ok {
			for key, val := range raw {
				obj[d.resolveKey(key)] = d.resolve(val)
			}
		}
		return obj
	case turboTypeSet:
		arr := make([]any, 0, len(value)-1)
		d.memo[index] = arr
		for i := 1; i < len(value); i++ {
			arr = append(arr, d.resolve(value[i]))
		}
		return arr
	case turboTypeMap:
		obj := map[string]any{}
		d.memo[index] = obj
		for i := 1; i+1 < len(value); i += 2 {
			obj[fmt.Sprint(d.resolve(value[i]))] = d.resolve(value[i+1])
		}
		return obj
	case turboTypePrevResolve:
		out := arg(1)
		d.memo[index] = out
		return out
	case turboTypePromise:
		// 页面里的 Promise 数据(如实验开关)对归档没有意义。
		d.memo[index] = nil
		return nil
	case turboTypeError:
		msg := fmt.Sprint(arg(1))
		d.memo[index] = msg
		return msg
	default:
		out := make([]any, 0, len(value)-1)
		d.memo[index] = out
		for i := 1; i < len(value); i++ {
			out = append(out, d.resolve(value[i]))
		}
		return out
	}
}

// resolve 解析一个引用值。
func (d *turboDecoder) resolve(v any) any {
	switch x := v.(type) {
	case float64:
		return d.hydrate(int(x))
	case int:
		return d.hydrate(x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return d.hydrate(int(n))
		}
		return x
	case string, bool, nil:
		return x
	default:
		return x
	}
}

// resolveKey 解析对象键 "_N" 对应的真实键名。
func (d *turboDecoder) resolveKey(key string) string {
	if strings.HasPrefix(key, "_") {
		if n, err := strconv.Atoi(key[1:]); err == nil {
			if v := d.hydrate(n); v != nil {
				return fmt.Sprint(v)
			}
		}
	}
	return key
}

// isRefHole 判断数组元素是否为空洞(-1)。
func isRefHole(v any) bool {
	if n, ok := v.(float64); ok {
		return int(n) == turboHole
	}
	return false
}
