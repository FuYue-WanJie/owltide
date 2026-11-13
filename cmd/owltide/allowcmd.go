package main

// Session-allow slash commands (P2b-M15): "本会话允许" management.
// Signatures are exact argv (tool.CommandSignature); risk-set commands
// are refused by perm.Service and never coverable.

import (
	"fmt"
	"strings"

	"github.com/FuYue-WanJie/owltide/core/tool"
	"github.com/FuYue-WanJie/owltide/internal/perm"
)

// handleAllowCommand handles chat slash commands for the session
// allowlist. Returns false when the line is not an /allow command (the
// caller sends it to the model as usual).
func handleAllowCommand(svc *perm.Service, line string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 || fields[0] != "/allow" {
		return false
	}
	const usage = "usage: /allow list | /allow add <command> | /allow revoke <command> | /allow revoke-all"
	if len(fields) < 2 {
		fmt.Println(usage)
		return true
	}
	switch fields[1] {
	case "list":
		all := svc.Allows()
		if len(all) == 0 {
			fmt.Println("（本会话暂无授权命令）")
			return true
		}
		fmt.Println("本会话授权命令：")
		for _, sig := range all {
			fmt.Println("  " + displaySig(sig))
		}
	case "add":
		cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/allow add"))
		if cmd == "" {
			fmt.Println(usage)
			return true
		}
		if err := svc.Allow(tool.CommandSignature(cmd), tool.IsRiskCommand(cmd)); err != nil {
			fmt.Printf("拒绝：该命令属于不可覆盖的风险集合，每次都会单独确认（%v）\n", err)
			return true
		}
		fmt.Println("已加入本会话授权：", cmd)
	case "revoke":
		cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/allow revoke"))
		if cmd == "" {
			fmt.Println(usage)
			return true
		}
		sig := tool.CommandSignature(cmd)
		if svc.Revoke(sig) || svc.Revoke(cmd) {
			fmt.Println("已撤销本会话授权：", cmd)
		} else {
			fmt.Println("未找到该授权：", cmd)
		}
	case "revoke-all":
		svc.RevokeAll()
		fmt.Println("已清空本会话授权")
	default:
		fmt.Println(usage)
	}
	return true
}

// displaySig renders a signature back into human-readable form.
func displaySig(sig string) string {
	calls := strings.Split(sig, "\x01")
	for i, c := range calls {
		calls[i] = strings.ReplaceAll(c, "\x00", " ")
	}
	return strings.Join(calls, " && ")
}
