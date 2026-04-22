# 4 节点联调脚本说明

这个目录包含当前 4 节点背书联调环境的启动脚本：

- `node1_redeploy.sh`
- `node2_start.sh`
- `node3_start.sh`
- `node4_start.sh`

这套脚本适用于下面的拓扑：

- `node-1` 运行 `nitro` 主节点
- `node-2`、`node-3`、`node-4` 分别运行一个 `nitro-val` 和一个 `endorser`

脚本默认使用内网 IP 做节点间通信，但 `node-1` 的脚本支持运行时覆盖地址，方便以后迁移到其他服务器环境。

## 总启动顺序

当前这套环境建议按下面顺序启动：

1. `node-2`
2. `node-3`
3. `node-4`
4. `node-1`

前三台先启动本机的 `nitro-val` 和 `endorser`。等它们都健康后，再启动 `node-1`，由 `node-1` 拉取三个背书节点的 BLS 公钥并启动 sequencer。

## 拓扑信息

当前实验环境的地址如下：

- `node-1`
  - 内网 IP：`192.168.1.9`
  - 公网 IP：`111.186.57.225`
- `node-2`
  - 内网 IP：`192.168.1.13`
  - 公网 IP：`111.186.57.180`
- `node-3`
  - 内网 IP：`192.168.1.6`
  - 公网 IP：`111.186.57.228`
- `node-4`
  - 内网 IP：`192.168.1.4`
  - 公网 IP：`111.186.57.231`

默认端口如下：

- `node-2`：`nitro-val=52000`，`endorser=9001`
- `node-3`：`nitro-val=52001`，`endorser=9002`
- `node-4`：`nitro-val=52002`，`endorser=9003`

## 每个脚本的作用

### `node1_redeploy.sh`

在 `node-1` 上运行。

它会：

1. 按需清空 `node-1` 的旧链数据。
2. 等待三个背书节点的 `/healthz` 可用。
3. 从三个背书节点的 `/pubkey` 接口获取 BLS 公钥。
4. 启动 `nitro`，并开启：
   - 禁用 L1 listener
   - 禁用 staker
   - 启用本地 dev wallet 资金
   - 启用远程 endorsement experiment
   - 接入三个验证节点的 RPC 地址
5. 输出 chain ID、当前块高和资助账户余额。

### `node2_start.sh`、`node3_start.sh`、`node4_start.sh`

分别在对应机器上运行。

每个脚本会：

1. 停掉本机已有的 `nitro-val` 和 `endorser`
2. 启动本机的 `nitro-val`
3. 启动本机的 `endorser`
4. 等待服务起来
5. 输出背书节点健康状态和公钥

## 前置条件

### 所有服务器都需要

- `curl`
- `jq`
- `cast`
- 已解压好的运行时包，路径为 `/data/endorsement`

运行时包中应该包含：

- `/data/endorsement/bin/nitro`
- `/data/endorsement/bin/nitro-val`
- `/data/endorsement/bin/endorser`
- `/data/endorsement/machines/latest`

### `node-1` 额外需要

- `node-2`、`node-3`、`node-4` 已经启动并可访问
- 四台机器都已经放好了 `val.jwt`
- `node-2`、`node-3`、`node-4` 上的 `bls.hex` 已经准备好

## 编译与打包

如果你要从源码重新准备运行时包，建议在一台可联网的 `x86_64 Linux` 机器上完成。

### 1. 安装依赖

```bash
sudo apt-get update
sudo apt-get install -y \
  git curl make build-essential clang lld cmake python3 unzip jq xxd \
  netcat-traditional nodejs npm wabt

sudo npm install -g yarn
```

### 2. 安装 Go

建议使用 Go 1.25.x。可以用系统包，也可以用本地安装版，只要和构建机环境一致即可。

### 3. 拉取仓库

```bash
git clone --branch feature/cloud-4node-experiments --recurse-submodules \
  git@github.com:SuzumiyaHaruki/endorsement.git
cd endorsement
```

如果没有 SSH 配置，也可以改成 HTTPS。

### 4. 编译二进制

```bash
make build-replay-env
go build -o target/bin/endorser ./cmd/endorser
```

### 5. 打包运行时

```bash
rm -rf /tmp/endorsement-runtime
mkdir -p /tmp/endorsement-runtime
cp -r target/bin /tmp/endorsement-runtime/
cp -r target/machines /tmp/endorsement-runtime/

tar -C /tmp/endorsement-runtime -czf /tmp/endorsement-runtime.tar.gz .
```

### 6. 分发到服务器

把 `tar.gz` 拷到四台机器上，然后在每台机器上解压到 `/data/endorsement`。

## 首次部署

在每台服务器上执行：

```bash
mkdir -p /data/endorsement /data/nitro-data /data/nitro-logs
tar -xzf /data/endorsement-runtime.tar.gz -C /data/endorsement
```

如果以后要更新运行时包，重新拷贝 tar 包并解压即可。

## 如何运行

### node-2

```bash
bash /data/node2_start.sh
```

### node-3

```bash
bash /data/node3_start.sh
```

### node-4

```bash
bash /data/node4_start.sh
```

### node-1

```bash
bash /data/node1_redeploy.sh
```

## 运行时可覆盖的环境变量

### `node1_redeploy.sh`

这个脚本支持通过环境变量覆盖地址和行为，不需要改脚本本身。

#### 是否清空旧链数据

- 默认：`RESET_CHAIN=1`
- 如果想保留旧链数据，执行时设置 `RESET_CHAIN=0`

示例：

```bash
RESET_CHAIN=0 bash /data/node1_redeploy.sh
```

#### 资助账户私钥

- 默认使用脚本内置的 dev wallet 私钥

#### 背书节点地址

如果未来迁移到别的网络，可以在运行时覆盖：

```bash
PEER_A_RPC=http://192.168.1.13:9001
PEER_B_RPC=http://192.168.1.6:9002
PEER_C_RPC=http://192.168.1.4:9003
PEER_A_VAL=ws://192.168.1.13:52000
PEER_B_VAL=ws://192.168.1.6:52001
PEER_C_VAL=ws://192.168.1.4:52002
```

### `node2_start.sh`、`node3_start.sh`、`node4_start.sh`

这三个脚本也支持少量覆盖项：

- `NODE_DATA_DIR`
- `LOG_DIR`
- `MACHINES_DIR`
- `JWT_SECRET`
- `BLS_SECRET_FILE`
- `VAL_PORT`
- `ENDORSER_PORT`
- `ENDORSER_ID`

当前实验环境默认值已经匹配好，一般不需要改。

## 验证

脚本执行完以后，可以这样检查 `node-1`：

```bash
cast chain-id --rpc-url http://127.0.0.1:8547
cast block-number --rpc-url http://127.0.0.1:8547
```

如果需要确认资助账户余额，可以执行：

```bash
cast wallet address --private-key 0xb6b15c8cb491557369f3c7d2c287b053eb229daa9c22138887752191c9520659
cast balance 0x3f1Eae7D46d88F08fc2F8ed27FCb2AB183EB2d0E --rpc-url http://127.0.0.1:8547
```

## 注意事项

- 这套脚本是为当前 4 节点联调环境准备的。
- `node-1` 依赖三个背书节点的内网可达性。
- 如果 `node-1` 拉不到公钥，通常是安全组没放通或者某个背书节点没启动。
