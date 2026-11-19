// chat.go OpenAI Chat Completions（POST /v1/chat/completions）同构透传层。
//
// 与 messages/responses 不同：客户端发来的就是 Chat 请求，内层期望的也是 Chat 请求，
// 无需协议转换。本层只做两件事：
//   1. 模型名解析——让 model_map 别名、custom_models 多候选别名、裸名（default_channel）
//      在 Chat 协议下也能用（内层 server.Handler 不认这些，裸别名会报 must use explicit prefix）；
//   2. 候选头注入——多候选别名时把 [alias, cand1, cand2...] 交给内层启用跨渠道切号。
//
// 普通模型（已带 channel/ 前缀且非别名）零回归：body 不改写、响应原样透传，
// 调用栈与「内层直接服务」时完全一致。
package gateway

import (
	"io"
	"net/http"
)

// handleChatCompletions 处理 POST /v1/chat/completions。
func (g *Gateway) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, ok := parseJSONBody(w, r, false)
	if !ok {
		return
	}
	model := asString(body["model"])

	// 自动别名（deva-main/deva-free 等）的 max_tokens 下限兜底：思考模型过小会正文空。
	// 必须在 resolveModel 之前用原始别名判断，且 body 会原样透传给内层，直接改 body 即可。
	g.applyAliasFloor(body, model)

	// 先判断是否多候选别名：是则直接用原始别名解析 + 注入完整候选阵列。
	candidates := g.candidatesFromBody(model)
	if len(candidates) > 0 {
		// 别名：body.model 换成解析后的第一候选实体（Router.Resolve 对 alias 返回排序第一项），
		// 内层普通模式也能正确路由；候选头负责后续实体用尽时切号。
		resolved, ok := g.resolveModel(w, model, false)
		if !ok {
			return
		}
		body["model"] = resolved
	} else {
		// 普通模型：同样走一次 Resolve，让 model_map 通配/裸名默认渠道生效。
		// 若解析结果与请求原名一致（已带 channel/ 前缀的常见情况），body 不改，保证零回归。
		resolved, ok := g.resolveModel(w, model, false)
		if !ok {
			return
		}
		if resolved != model {
			body["model"] = resolved
		}
	}

	stream := asBool(body["stream"])
	res, err := g.callChat(w, r, body, stream, candidates)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_unavailable", err.Error())
		return
	}
	if stream {
		g.streamChatRaw(w, res)
		return
	}

	status, raw, err := readChatResponse(res)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_read", err.Error())
		return
	}
	if status >= 400 {
		writeUpstreamError(w, status, raw, false)
		return
	}
	// 同构透传：内层给的就是标准 Chat 响应，原样写回，不 decode/重建，
	// 避免重新序列化丢字段（tool_calls / reasoning_content / usage 细节等）。
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// streamChatRaw 把内层 Chat SSE 原样转发给客户端。Chat 是同构协议，
// 不需要像 Anthropic/Responses 那样逐 chunk 转换，直接拷贝字节即可。
func (g *Gateway) streamChatRaw(w http.ResponseWriter, res *innerResult) {
	defer res.Close()
	if res.Status >= 400 {
		raw, _ := res.ReadAll()
		writeUpstreamError(w, res.Status, raw, false)
		return
	}
	flush := sseHeader(w)
	_, _ = io.Copy(w, res.Body)
	flush()
}
