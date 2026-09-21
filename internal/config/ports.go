package config

import (
	"net"
	"strconv"
)

// browserBlockedPorts 是 Chromium 的保留端口表，照抄自
// net/base/port_util.cc 的 kRestrictedPorts。
//
// 为什么值得单独维护一份：这些端口的拦截发生在**浏览器内部**，
// 请求根本不会发出去 —— 服务端看不到任何连接、日志里干干净净，
// 前端只会显示 ERR_UNSAFE_PORT。于是排查会一路跑到防火墙、端口映射、
// 容器网络上去，唯独想不到是端口号本身的问题。
//
// Chrome / Edge / Brave / Opera 共用这份列表；Firefox 也拦，只是报错文案不同
// （「This address uses a network port which is normally used for purposes
// other than Web browsing」），且只能靠 about:config 逐机放行。
//
// 注意：`--explicitly-allowed-ports` 与组策略 ExplicitlyAllowedNetworkPorts
// 只能放开**本机**浏览器，其他访问者照样打不开，不能作为部署方案。
var browserBlockedPorts = map[int]string{
	1:     "tcpmux",
	7:     "echo",
	9:     "discard",
	11:    "systat",
	13:    "daytime",
	15:    "netstat",
	17:    "qotd",
	19:    "chargen",
	20:    "ftp data",
	21:    "ftp access",
	22:    "ssh",
	23:    "telnet",
	25:    "smtp",
	37:    "time",
	42:    "name",
	43:    "nicname",
	53:    "domain",
	69:    "tftp",
	77:    "priv-rjs",
	79:    "finger",
	87:    "ttylink",
	95:    "supdup",
	101:   "hostriame",
	102:   "iso-tsap",
	103:   "gppitnp",
	104:   "acr-nema",
	109:   "pop2",
	110:   "pop3",
	111:   "sunrpc",
	113:   "auth",
	115:   "sftp",
	117:   "uucp-path",
	119:   "nntp",
	123:   "NTP",
	135:   "loc-srv / epmap",
	137:   "netbios",
	139:   "netbios",
	143:   "imap2",
	161:   "snmp",
	179:   "BGP",
	389:   "ldap",
	427:   "SLP",
	465:   "smtp+ssl",
	512:   "print / exec",
	513:   "login",
	514:   "shell",
	515:   "printer",
	526:   "tempo",
	530:   "courier",
	531:   "chat",
	532:   "netnews",
	540:   "uucp",
	548:   "AFP",
	554:   "rtsp",
	556:   "remotefs",
	563:   "nntp+ssl",
	587:   "smtp",
	601:   "syslog-conn",
	636:   "ldap+ssl",
	989:   "ftps-data",
	990:   "ftps",
	993:   "ldap+ssl",
	995:   "pop3+ssl",
	1719:  "h323gatestat",
	1720:  "h323hostcall",
	1723:  "pptp",
	2049:  "nfs",
	3659:  "apple-sasl",
	4045:  "lockd",
	5060:  "sip",
	5061:  "sips",
	6000:  "X11",
	6566:  "sane-port",
	6665:  "alternate IRC",
	6666:  "alternate IRC",
	6667:  "IRC",
	6668:  "alternate IRC",
	6669:  "alternate IRC",
	6697:  "IRC + TLS",
	10080: "Amanda",
}

// CheckListenPort 解析监听地址，判断该端口是否被浏览器保留。
//
// 返回的 blocked 为 true 表示：管理界面用浏览器**永远打不开**，
// 与防火墙、端口映射、容器网络均无关。service 是对应的传统服务名
// （用于把「为什么被拦」解释清楚）。
//
// 地址无法解析或端口越界时返回 blocked=false —— 那是另一类错误，
// 交给 net.Listen 去报，不在这里越权判断。
func CheckListenPort(listen string) (port int, service string, blocked bool) {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return 0, "", false
	}
	n, err := strconv.Atoi(portStr)
	if err != nil || n < 1 || n > 65535 {
		return 0, "", false
	}
	svc, ok := browserBlockedPorts[n]
	if !ok {
		return n, "", false
	}
	return n, svc, true
}
