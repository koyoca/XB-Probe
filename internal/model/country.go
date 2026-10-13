package model

import "strings"

// 节点的"国家/地区"代码（ISO 3166-1 alpha-2）校验与规范化。
//
// 为什么单独抽出来：这个值有三个入口（管理端 PATCH、后台表单、未来的导入脚本），
// 每个入口都自己写一遍大小写/长度判断，迟早会出现"一个入口存了 hk、另一个存了 HK"，
// 前端查表时一半节点没有旗帜——而且这种不一致在数据里看不出来（都是字符串）。

// CountryCodeLen 是国家代码长度（ISO 3166-1 alpha-2）。
const CountryCodeLen = 2

// NormalizeCountry 规范化国家代码：去空白、转大写；不合法时返回空串。
//
// 返回空串是"未设置"的统一表示（NULL 与空串等价），而不是错误——
// 调用方据此决定是清空该字段还是写入。
func NormalizeCountry(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if !IsValidCountryCode(s) {
		return ""
	}
	return s
}

// IsValidCountryCode 判断是否形如两位大写字母。
//
// 刻意**不**校验"是否真实存在的国家代码"：国家/地区列表会变（新增、更名、代码调整），
// 把白名单硬编码在服务端意味着每次都要发布新版本；而国旗渲染在前端本来就是查表，
// 查不到的代码会落到"未知地区"占位，不会画出错旗。服务端只守住"形态"这一层。
func IsValidCountryCode(s string) bool {
	if len(s) != CountryCodeLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return true
}
