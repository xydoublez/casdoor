#!/bin/bash
# 使用 radtest 验证 Casdoor RADIUS 认证（PAP 方式，Casdoor RADIUS 仅支持 PAP）
#
# 用法:
#   ./test_radius.sh                    # 测试默认账号 admin/123
#   ./test_radius.sh <用户名> <密码>     # 测试指定账号
#   ./test_radius.sh admin 123 neg      # 追加负向测试（错误密码应被拒绝）
#
# 环境变量:
#   RADIUS_HOST=127.0.0.1   # Casdoor 地址
#   RADIUS_PORT=1812        # RADIUS 端口(UDP)
#   RADIUS_SECRET=secret    # 共享密钥（须与 start_pg.sh 的 RADIUS_SECRET 一致）
#
# 退出码: 0=认证成功 1=拒绝 2=Challenge(MFA) 3=环境问题 4=无响应

RADIUS_HOST="${RADIUS_HOST:-127.0.0.1}"
RADIUS_PORT="${RADIUS_PORT:-1812}"
RADIUS_SECRET="${RADIUS_SECRET:-secret}"

TEST_USER="${1:-admin}"
TEST_PASS="${2:-123}"
NEGATIVE="${3:-}"

if ! command -v radtest > /dev/null 2>&1; then
    echo "!! 未安装 radtest，请先安装: sudo apt-get install -y freeradius-utils"
    exit 3
fi

# 单次认证测试: $1=密码 $2=测试说明 $3=预期结果(accept|reject)
run_test() {
    local out rc expect
    expect="${3:-accept}"
    out="$(radtest -t pap "${TEST_USER}" "$1" "${RADIUS_HOST}:${RADIUS_PORT}" 0 "${RADIUS_SECRET}" 2>&1)"
    echo "${out}"
    echo "----------------------------------------------"
    # 只匹配 "Received ..." 响应行，避免 radtest 诊断信息 "Expected Access-Accept got Access-Reject" 干扰
    if echo "${out}" | grep -q "Received Access-Accept"; then
        if [ "${expect}" = "accept" ]; then
            echo ">> [PASS] $2: 用户 ${TEST_USER} RADIUS 认证成功"
            rc=0
        else
            echo ">> [FAIL] $2: 错误密码竟然被接受，请检查服务端配置！"
            rc=1
        fi
    elif echo "${out}" | grep -q "Received Access-Challenge"; then
        echo ">> [CHALLENGE] $2: 用户 ${TEST_USER} 启用了 MFA，radtest 无法交互输入 OTP（服务端行为正常）"
        rc=2
    elif echo "${out}" | grep -q "Received Access-Reject"; then
        if [ "${expect}" = "reject" ]; then
            echo ">> [PASS] $2: 错误密码被正确拒绝"
            rc=0
        else
            echo ">> [FAIL] $2: 用户 ${TEST_USER} 认证被拒绝"
            rc=1
        fi
    else
        echo ">> [ERROR] $2: 未收到 Access-Accept/Reject，请确认 Casdoor RADIUS(${RADIUS_HOST}:${RADIUS_PORT}) 已启动且密钥一致"
        rc=4
    fi
    return "${rc}"
}

echo ">> RADIUS 认证测试: ${TEST_USER} @ ${RADIUS_HOST}:${RADIUS_PORT}（PAP）"
echo "----------------------------------------------"
run_test "${TEST_PASS}" "正向测试" accept
rc=$?

if [ "${NEGATIVE}" = "neg" ]; then
    echo ""
    echo ">> 负向测试: 使用错误密码，预期 Access-Reject"
    echo "----------------------------------------------"
    run_test "${TEST_PASS}-wrong" "负向测试" reject
    nrc=$?
    if [ "${rc}" -eq 0 ] && [ "${nrc}" -eq 0 ]; then
        echo ">> 结论: 认证逻辑正常（正确密码通过，错误密码拒绝）"
        exit 0
    fi
    exit 1
fi

exit "${rc}"
