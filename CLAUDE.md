# CLAUDE.md — faultfs 仓库协作约定

## Git 提交规范（重要）

- **不要在 git 提交中添加 Claude / Anthropic 的任何模型作为作者或共著者**。
  即：不要写 `Co-Authored-By: Claude ...`（无论 "Claude Code"、"Claude Fable 5" 还是其它
  Claude/Anthropic 模型名），也不要把提交作者（author/committer）设成 Claude。
- 提交作者沿用仓库现有作者（`ryjer`）；提交信息正常写功能/修复说明即可。
- 原因：本仓库的开发未使用 Claude 模型，提交署名应反映真实的作者情况，避免误导。

适用范围：`git commit`、`git merge`/`rebase` 产生的提交、PR 描述等一切署名处。
