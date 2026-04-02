package failover

import "encoding/base64"

var maskKey = []byte("llm_failover_mask_key")

// SetMaskKey 设置渠道名混淆密钥。
func SetMaskKey(key string) {
	if key != "" {
		maskKey = []byte(key)
	}
}

// MaskChannelName 混淆渠道标识，避免直接暴露真实渠道名。
func MaskChannelName(name string) string {
	if name == "" {
		return ""
	}
	data := []byte(name)
	buf := make([]byte, len(data))
	for i := 0; i < len(data); i++ {
		buf[i] = data[i] ^ maskKey[i%len(maskKey)]
	}
	return base64.URLEncoding.EncodeToString(buf)
}

// UnmaskChannelName 还原被混淆的渠道标识。
func UnmaskChannelName(masked string) string {
	if masked == "" {
		return ""
	}
	data, err := base64.URLEncoding.DecodeString(masked)
	if err != nil {
		return ""
	}
	buf := make([]byte, len(data))
	for i := 0; i < len(data); i++ {
		buf[i] = data[i] ^ maskKey[i%len(maskKey)]
	}
	return string(buf)
}
