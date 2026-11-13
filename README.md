# 存储卡取证工作站

浏览器内运行的只读存储卡取证工具，面向行车记录仪 SD 卡的删除/覆盖恢复场景。
纯 TypeScript 实现，解析、哈希、恢复、雕刻全部在本机完成，镜像不上传。

## 功能

| 能力 | 实现要点 |
| --- | --- |
| 导入 dd 镜像 | 支持裸卷、MBR（含扩展分区）、GPT（含 CRC 校验）；按 512B 步进探测卷偏移 |
| FAT32 / exFAT | 独立解析引导区、FSInfo、分配位图、目录项；大 FAT 自动改为按需读取 |
| 现存 + 已删除文件 | 目录遍历保留原始偏移；删除条目标记、目录删除导致的孤儿项 |
| 长文件名恢复 | FAT LFN 片段校验和与物理倒序重组；exFAT Name 项 + NameHash 校验 |
| 按簇链恢复 | FAT 链残留下按链恢复；链被清零则按连续簇假设，并明确标注依据 |
| 完整性标注 | 链断裂、越界、成环、数据簇被重新分配 → 完整/不完整/已覆盖三态结论 |
| 未分配空间雕刻 | JPEG 逐段解析至 EOI、MP4 按 box 结构（ftyp/mdat/moov）重建对象边界 |
| 哈希 | 整镜像流式 MD5 + SHA-256；恢复文件与雕刻对象逐项计算 |
| 时间线 | FAT 无时区按案件时区解释、exFAT 用磁盘显式偏移；访问时间标“仅日期” |
| 监管链 | 每条记录含前序哈希，形成可独立复核的 SHA-256 链 |
| PDF 报告 | 内嵌 Noto Sans SC 子集，中文可复制、可检索 |

## 快速开始

```bash
npm install
npm run fixtures        # 生成含已知答案的 FAT32/exFAT 测试镜像（需要系统 ffmpeg）
npm run dev             # http://localhost:5173
```

页面顺序：案件 → 镜像分析 → 文件恢复 → 时间线 → 报告。

## 测试

```bash
npm test                # 核心单元与集成测试（42 项）
npm run test:e2e        # Playwright 浏览器端到端（真实 Worker + 真实镜像）
npm run typecheck
npm run build
```

测试镜像由 `tests/fixtures/*-builder.ts` 独立实现生成（不复用被测解析器），
并用系统工具做外部交叉校验：

```bash
fsck.fat -n fixtures/dashcam-fat32.dd          # dosfstools
hdiutil attach -nomount -imagekey diskimage-class=CRawDiskImage fixtures/dashcam-exfat.img
fsck_exfat -n /dev/diskN                        # macOS fsck_exfat
```

`fixtures/*.answers.json` 记录每个文件的 SHA-256/MD5、簇号、期望恢复状态，
单元测试与端到端测试都以它为“已知答案”。

## 已实测的规模

在 macOS / Chromium 上对稀疏镜像实测（SSD，文件实际仅占几十 KB）：

| 镜像 | 全盘 MD5+SHA-256 | 目录扫描 | 结果 |
| --- | --- | --- | --- |
| 4 GiB FAT32 | 约 27 s | 0.1 s | 正常 |
| 32 GiB FAT32 | 约 223 s（≈150 MB/s） | 0.1 s | 正常，无报错 |

32GB 卡的哈希时间主要花在顺序读盘；解析本身不随容量线性增长，因为
FAT 大于 96 MiB 时改为按需读取，目录扫描只触及根目录与其链。

## 取证语义（重要）

- **只读**：所有读取经 `ByteSource` 抽象，没有任何写接口。`tests/core/readonly.test.ts`
  跑完整流程后比对前后哈希，作为回归保证。
- **完整性不粉饰**：恢复结果只有 `complete / partial / overwritten / failed` 四种，
  并附带缺失字节数、覆盖簇数、恢复方法与依据。簇链被清零时恢复出的字节可能正确，
  但报告会写明“按连续簇假设，依据是数据区未被重新分配”。
- **时间戳精度**：FAT/exFAT 的访问时间只记录日期，工具显示“仅日期”，
  不给它编造 00:00 之外的时刻；FAT 时间无时区，按案件时区解释并标注。
- **监管链定位**：软件级哈希链可检测记录被改动，但不等同于可信时间戳或数字签名。
  对外举证建议另行公证或接入时间戳服务。
- **重要提醒**：工具只能保证“不修改镜像”，无法挽回已被相机/系统覆盖的数据；
  送检卡应先写保护，再做镜像。

## 第三方组件

- [pdf-lib](https://github.com/Hopding/pdf-lib) + [@pdf-lib/fontkit](https://github.com/Hopding/fontkit) — PDF 生成与字体子集嵌入（MIT）
- [hash-wasm](https://github.com/Daninet/hash-wasm) — WASM 加速 MD5/SHA-256（MIT）
- [idb](https://github.com/jakearchibald/idb) — IndexedDB 封装（ISC）
- 报告字体 `NotoSansSC-Regular.ttf` 为 Noto Sans SC 子集（GB2312 + 常用符号），
  依 SIL Open Font License 1.1 分发，许可证随附于 `public/fonts/OFL.txt`。

## 目录结构

```
src/core/          解析与取证内核（纯函数式、无 DOM 依赖）
  disk.ts          MBR / GPT / 裸卷探测
  fs/fat.ts        FAT12/16/32
  fs/exfat.ts      exFAT
  recover.ts       恢复编排与完整性判定
  carve.ts         JPEG / MP4 雕刻
  timeline.ts      时间线归一化
  custody.ts       监管链哈希链
  report/pdf.ts    PDF 报告
  analyze.ts       端到端编排
src/workers/       分析 Worker（哈希/扫描/恢复/雕刻）
src/lib/           IndexedDB、监管链服务、状态中枢
src/pages/         五个工作页面
tests/fixtures/    独立实现的镜像生成器与已知答案
tests/core/        单元与集成测试
tests/e2e/         浏览器端到端测试
```
