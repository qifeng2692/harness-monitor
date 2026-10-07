// 增量续读（live 跟随）的通用工具。
//
// 设计目标：live 刷新只处理“新增行”，不重复扫描已入库的历史（大文件如 172 MB
// 第二遍全扫虽幂等但耗时）。做法是把每个数据源文件已消费到的字节 offset / 行数
// 记入 store 的 ingest_state 表，下一轮从 offset 续读。
//
// 关键不变量：offset 恰好停在某个换行之后——即只消费“以换行结尾的完整行”，
// 末尾可能仍在写入的半行留给下一轮。这样续读永远从行首开始，不会读到半行。
package collect

import (
	"bufio"
	"bytes"

	"github.com/qifeng2692/harness-monitor/internal/model"
)

// splitCompleteLines 是 bufio.Scanner 的分割函数：只产出以换行结尾的完整行。
//
// 与默认的 bufio.ScanLines 不同，末尾残留的、无换行结尾的半行（写入中）不被消费，
// 直接以 bufio.ErrFinalToken 结束扫描——保证调用方按 advance 累计的字节数恰好停在
// 换行之后。返回的 token 含行尾 \r（CRLF 场景），由调用方自行 TrimRight。
//
// 实测本机所有数据源文件均以 \n 结尾，故该差异对全量采集的输出无影响。
func splitCompleteLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF {
		return 0, nil, bufio.ErrFinalToken
	}
	return 0, nil, nil // 需要更多数据
}

// resolveStart 计算某文件本轮应从哪个位置开始读，并返回是否处于增量模式。
//
//	states == nil（全量模式）→ 从 0 起读，incremental=false；
//	已知文件（有 ingest_state）→ 从上次 offset 起读；
//	未知文件（新增）→ 从 0 起读（补历史，保证不丢数据）。
//
// 文件被截断/轮转（当前 size < 已存 offset）时从 0 重新读。
func resolveStart(rel string, states map[string]model.IngestPos, size int64) (model.IngestPos, bool) {
	if states == nil {
		return model.IngestPos{Size: size}, false
	}
	pos, ok := states[rel]
	if !ok || pos.Offset > size {
		// 新增文件，或文件被截断/轮转：从头读。
		return model.IngestPos{Size: size}, true
	}
	pos.Size = size
	return pos, true
}
