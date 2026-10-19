// 导出脱敏：把会话内容里的敏感片段替换为占位符，让导出文件可以直接分享。
//
// 背景（对标 akmon 的 redact 能力）：hm export 此前原文直出，而 README
// 宣传导出文件「可直接阅读、归档或分享」——但事件内容里大量出现本机个人
// 路径（C:\Users\QF\...）与各类凭据（ghp_...、sk-...、Bearer ...），原文
// 分享等于把隐私一起带走。testdata 清理时已为此手工清过三种形态的路径。
//
// 原则：只做模式替换，不做内容理解；宁多脱不错漏（--no-redact 可关）。
// 全部为纯函数，可单测。

package export

import (
	"regexp"
	"strconv"
)

// 脱敏占位符。同一类敏感信息共用一个占位符，避免占位符本身泄露类别外的信息。
const (
	phUserPath = "[已脱敏:用户路径]"
	phEmail    = "[已脱敏:邮箱]"
	phSecret   = "[已脱敏:密钥]"
)

// 规则按「上下文强、易误伤少」的先跑：带上下文的（token=...、Bearer ...）
// 先整段吞掉值，裸模式的（ghp_...、邮箱）后跑兜底，避免占位符被二次替换。
var (
	// 键值对形态：token=xxx / api_key: xxx / password = "xxx"
	reKVSecret = regexp.MustCompile(`(?i)\b((?:api[_-]?key|secret|password|passwd|token)[_a-z-]*\s*[:=]\s*["']?)([^\s"',;\\]{4,})`)
	// Authorization 头 / Bearer 令牌
	reAuthHdr = regexp.MustCompile(`(?i)\b((?:authorization\s*:\s*(?:basic|bearer)?\s*|bearer\s+))[^\s"',;\\]{8,}`)
	// OpenAI / Anthropic 风格密钥（sk-...、sk-ant-...、sk-proj-...）
	reSkKey = regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{16,}`)
	// GitHub 令牌（ghp_/gho_/ghu_/ghs_/ghr_ 与细粒度 github_pat_）
	reGhToken   = regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`)
	reGitHubPat = regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`)
	// Slack 令牌
	reSlackTok = regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`)
	// AWS AccessKeyId
	reAWSKey = regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)
	// Windows 用户目录：C:\Users\x\...（连同后续路径段整段吞掉，避免泄露目录结构；
	// JSON 转义形态 C:\\Users\\x 由 [\\/]+ 自然覆盖）
	reWinPath = regexp.MustCompile(`(?i)[A-Z]:[\\/]+Users[\\/]+[^\s"'` + "`" + `:<>|]*`)
	// MSYS / git-bash 形态：/c/Users/x/...
	reMsysPath = regexp.MustCompile(`(?i)/[A-Z]/Users/[^\s"'` + "`" + `:<>|]*`)
	// Linux 用户目录：/home/x/...、/root/...
	reNixPath = regexp.MustCompile(`/(?:home|root)/[A-Za-z0-9._/-]*`)
	// 邮箱
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

type redactRule struct {
	re      *regexp.Regexp
	ph      string
	keepPre int // 保留第 1 个捕获组（上下文前缀，如 "token="），只替换其余部分
}

var redactRules = []redactRule{
	{re: reKVSecret, ph: phSecret, keepPre: 1},
	{re: reAuthHdr, ph: phSecret, keepPre: 1},
	{re: reSkKey, ph: phSecret},
	{re: reGhToken, ph: phSecret},
	{re: reGitHubPat, ph: phSecret},
	{re: reSlackTok, ph: phSecret},
	{re: reAWSKey, ph: phSecret},
	{re: reWinPath, ph: phUserPath},
	{re: reMsysPath, ph: phUserPath},
	{re: reNixPath, ph: phUserPath},
	{re: reEmail, ph: phEmail},
}

// Redact 把文本中的敏感内容替换为占位符，返回替换后的文本与替换总处数。
func Redact(s string) (string, int) {
	total := 0
	for _, r := range redactRules {
		n := 0
		s = r.re.ReplaceAllStringFunc(s, func(m string) string {
			n++
			if r.keepPre > 0 {
				sub := r.re.FindStringSubmatch(m)
				if len(sub) > r.keepPre {
					return sub[r.keepPre] + r.ph
				}
			}
			return r.ph
		})
		total += n
	}
	return s, total
}

// RedactNote 导出文首的脱敏说明（脱敏开启时写进头部）。
func RedactNote(n int) string {
	if n > 0 {
		return "- 脱敏：已开启（个人路径 / 邮箱 / 密钥 → [已脱敏:…]），本次替换 " + strconv.Itoa(n) + " 处；原文导出用 --no-redact\n"
	}
	return "- 脱敏：已开启（个人路径 / 邮箱 / 密钥 → [已脱敏:…]），本会话未发现需替换内容\n"
}
