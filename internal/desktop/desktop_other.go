//go:build !windows

package desktop

import (
	"log"
	"os"
	"os/exec"
	"runtime"
)

// 非 Windows 平台没有 WebView2，退到"用系统默认浏览器打开"。
//
// 为什么不在这里也做 --app 模式：macOS 的 Chrome 支持 `--app=`，
// 但 Linux 上各发行版的浏览器参数差异较大，而这两个平台的用户
// 本来就更习惯用浏览器标签页访问本地服务。少一层猜测。
func Run(opt Options) error {
	opt = opt.withDefaults()
	url := opt.URL

	if err := openDefaultBrowser(url); err != nil {
		log.Printf("INFO: 无法自动打开浏览器（%v），请手动访问 %s", err, url)
		if opt.OnReady != nil {
			opt.OnReady(ModeNone)
		}
	} else {
		log.Printf("INFO: 已在默认浏览器中打开 %s", url)
		if opt.OnReady != nil {
			opt.OnReady(ModeBrowserApp)
		}
	}
	// 非 Windows 下没有窗口可阻塞，直接返回（调用方据 OnClose 语义自行处理）。
	if opt.OnClose != nil {
		opt.OnClose()
	}
	return nil
}

func openDefaultBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		// xdg-open 在绝大多数桌面发行版上都可用
		if p, err := exec.LookPath("xdg-open"); err == nil {
			return exec.Command(p, url).Start()
		}
		if p, err := exec.LookPath("gio"); err == nil {
			return exec.Command(p, "open", url).Start()
		}
		return os.ErrNotExist
	}
}

// Available 在非 Windows 上恒为"用浏览器"。
func Available() (Mode, string) {
	if _, err := exec.LookPath("xdg-open"); err == nil || runtime.GOOS == "darwin" {
		return ModeBrowserApp, "非 Windows 平台：使用系统默认浏览器"
	}
	return ModeNone, "非 Windows 平台：请手动打开控制台地址"
}
