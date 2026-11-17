// Package customopenai 封装用户自定义的 OpenAI 兼容上游（D 任务）。
//
// 与内置渠道的关键差异：
//   - 无登录/签到/刷新：每个实例是装配期由 config.custom_channels 生成的**虚拟账号**，
//     base_url 存 auth.ApiHost，api_key 存 auth.AccessToken，FilePath 为空（不落盘 auth 文件）。
//   - 一个 Kind（CustomOpenAI）承载 N 个实例：client 的 baseURL 从账号字段读，故不同账号
//     可以指向完全不同的上游；这些账号同处一个 Pool，天然参与 A 自动选渠与 E 多候选轮换。
//   - 协议：标准 OpenAI Chat Completions（POST {base_url}/chat/completions，Bearer key，SSE）。
//     响应本身就是 OpenAI 形状，Stream/Aggregate 直接复用 internal/upstream 通用规范化层，
//     无需渠道私有转换器。
package customopenai

// ChannelName 是本渠道在统计/日志/模型前缀里的标识。
const ChannelName = "customopenai"

// EpChat 推理端点（拼在 base_url 之后）。base_url 用户在面板填写时通常带到 /v1，
// 故这里只补 /chat/completions；normalize 会保证 base_url 不带尾斜杠。
const EpChat = "/chat/completions"

// EpModels 模型目录端点（可选增强，一期未用；保留常量便于二期自动拉模型）。
const EpModels = "/models"
