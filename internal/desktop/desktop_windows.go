//go:build windows

package desktop

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// 本文件是 Windows 下的桌面窗口实现：Win32 窗口 + 自写的 WebView2 COM 绑定。
//
// 全程只用 stdlib（syscall / unsafe）—— 这是"零第三方依赖"这条约束的代价，
// 也是它的兑现方式。COM 交互的要点：
//
//   - 一个 COM 接口指针，指向的是**指向 vtable 的指针**。所以实现一个 COM 对象
//     时，结构体的**第一个字段**必须是 vtable 指针，而传给 COM 的是
//     `&结构体.第一个字段`。
//   - 调用方法 = 从 vtable 取第 N 个函数指针再调用，第 0 个参数是接口自身。
//   - 回调（handler）用 syscall.NewCallback 包装 Go 函数，并且**必须保持可达** ——
//     否则 GC 回收后 COM 侧调用会崩。见 comKeepAlive。

// ---- Win32 ----

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	ole32    = syscall.NewLazyDLL("ole32.dll")

	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procShowWindow       = user32.NewProc("ShowWindow")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procGetClientRect    = user32.NewProc("GetClientRect")
	procLoadCursorW      = user32.NewProc("LoadCursorW")
	procSetWindowTextW   = user32.NewProc("SetWindowTextW")
	procAdjustWindowRect = user32.NewProc("AdjustWindowRect")
	procGetSystemMetrics = user32.NewProc("GetSystemMetrics")
	procMessageBoxW      = user32.NewProc("MessageBoxW")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
)

const (
	wsOverlappedWindow = 0x00CF0000
	wsVisible          = 0x10000000
	swShow             = 5
	cwUseDefault       = 0x80000000
	idcArrow           = 32512
	smCXScreen         = 0
	smCYScreen         = 1

	wmDestroy = 0x0002
	wmSize    = 0x0005
	wmClose   = 0x0010
	wmApp     = 0x8000 // WM_APP
	// wmRunOnUI 用于把"在 UI 线程上执行某个操作"投递过去。
	wmRunOnUI = wmApp + 1

	coinitApartmentThreaded = 0x2
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     syscall.Handle
	hIcon         syscall.Handle
	hCursor       syscall.Handle
	hbrBackground syscall.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       syscall.Handle
}

type rect struct{ Left, Top, Right, Bottom int32 }

type point struct{ X, Y int32 }

type msg struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
	// lPrivate 是 Windows 8+ 的额外字段，保持结构体大小与系统一致
	lPrivate uint32
}

// ---- COM 基础设施 ----

// comKeepAlive 持有所有交给 COM 的 Go 对象，防止被 GC 回收。
//
// 这是**必须**的：syscall.NewCallback 生成的函数指针被 COM 侧长期持有，
// 若对应的 Go 对象被回收，后续回调会跳到已释放的内存上，表现为随机崩溃。
var (
	comMu        sync.Mutex
	comKeepAlive []any
)

func comRetain(v any) {
	comMu.Lock()
	comKeepAlive = append(comKeepAlive, v)
	comMu.Unlock()
}

// comCall 按 vtable 索引调用 COM 方法。
func comCall(iface unsafe.Pointer, index int, args ...uintptr) uintptr {
	if iface == nil {
		return 0x80004003 // E_POINTER
	}
	vtbl := *(*unsafe.Pointer)(iface)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(index)*unsafe.Sizeof(uintptr(0))))
	full := make([]uintptr, 0, len(args)+1)
	full = append(full, uintptr(iface))
	full = append(full, args...)
	ret, _, _ := syscall.SyscallN(fn, full...)
	return ret
}

func comRelease(iface unsafe.Pointer) {
	if iface != nil {
		comCall(iface, 2) // Release
	}
}

// comHandler 是一个最小 COM 对象：IUnknown + 一个 Invoke。
//
// ⚠️ lpVtbl 必须是**第一个字段**，且传给 COM 的是 &h.lpVtbl。
type comHandler struct {
	lpVtbl unsafe.Pointer
	vtbl   [4]uintptr

	refs   int32
	invoke func(hr, arg uintptr)
	// ptr 是接口指针（&lpVtbl），缓存下来便于传递与比较。
	ptr uintptr
}

func newCOMHandler(invoke func(hr, arg uintptr)) *comHandler {
	h := &comHandler{invoke: invoke}
	h.vtbl[0] = syscall.NewCallback(h.queryInterface)
	h.vtbl[1] = syscall.NewCallback(h.addRef)
	h.vtbl[2] = syscall.NewCallback(h.release)
	h.vtbl[3] = syscall.NewCallback(h.invokeImpl)
	h.lpVtbl = unsafe.Pointer(&h.vtbl[0])
	h.ptr = uintptr(unsafe.Pointer(&h.lpVtbl))
	comRetain(h)
	return h
}

func (h *comHandler) queryInterface(this, riid, ppv uintptr) uintptr {
	if ppv == 0 {
		return 0x80004003
	}
	*(*uintptr)(unsafe.Pointer(ppv)) = this
	h.addRef(this)
	return 0 // S_OK
}

func (h *comHandler) addRef(uintptr) uintptr {
	return uintptr(atomic.AddInt32(&h.refs, 1))
}

func (h *comHandler) release(uintptr) uintptr {
	n := atomic.AddInt32(&h.refs, -1)
	if n <= 0 {
		// 不真正释放：进程生命周期内一直持有（见 comKeepAlive 的说明）
		return 0
	}
	return uintptr(n)
}

func (h *comHandler) invokeImpl(_ uintptr, hr, arg uintptr) uintptr {
	if h.invoke != nil {
		h.invoke(hr, arg)
	}
	return 0
}

// ---- WebView2 加载 ----

// webview2LoaderCandidates 返回 WebView2Loader.dll 的候选路径。
//
// ⚠️ 这个 DLL **不由 WebView2 运行时提供**（实测：运行时目录里的
// EmbeddedBrowserWebView.dll 并不导出 CreateCoreWebView2EnvironmentWithOptions）。
// 它是 WebView2 SDK 的一部分，需要随应用分发。所以：
//   - 首选：与本 exe 同目录（用户把 DLL 拷过来即可）
//   - 其次：交给 LoadLibrary 按标准搜索顺序找（PATH、系统目录等）
//   - 再次：环境变量 TRAE2API_WEBVIEW2_LOADER 指定的路径
//
// 找不到时**不报错退出**，而是降级到浏览器 App 模式（见 Run）。
func webview2LoaderCandidates() []string {
	var out []string
	if p := strings.TrimSpace(os.Getenv("TRAE2API_WEBVIEW2_LOADER")); p != "" {
		out = append(out, p)
	}
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), "WebView2Loader.dll"))
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(wd, "WebView2Loader.dll"))
	}
	return out
}

// loadWebView2 依次尝试加载 WebView2Loader.dll，返回可用的 DLL 句柄。
func loadWebView2() (*syscall.LazyDLL, error) {
	var tried []string
	for _, p := range webview2LoaderCandidates() {
		if _, err := os.Stat(p); err != nil {
			tried = append(tried, p+"（不存在）")
			continue
		}
		dll := syscall.NewLazyDLL(p)
		if err := dll.Load(); err != nil {
			tried = append(tried, fmt.Sprintf("%s（%v）", p, err))
			continue
		}
		if err := dll.NewProc("CreateCoreWebView2EnvironmentWithOptions").Find(); err != nil {
			tried = append(tried, fmt.Sprintf("%s（缺导出）", p))
			continue
		}
		return dll, nil
	}
	// 最后试一次系统搜索路径
	if dll := syscall.NewLazyDLL("WebView2Loader.dll"); dll.Load() == nil {
		if dll.NewProc("CreateCoreWebView2EnvironmentWithOptions").Find() == nil {
			return dll, nil
		}
	}
	return nil, fmt.Errorf("未找到可用的 WebView2Loader.dll；已尝试：%s", strings.Join(tried, "；"))
}

// ---- 窗口 ----

// appState 是窗口的运行时状态（一个进程只开一个窗口）。
type appState struct {
	hwnd     syscall.Handle
	opts     Options
	webview  unsafe.Pointer // ICoreWebView2*
	ctrl     unsafe.Pointer // ICoreWebView2Controller*
	env      unsafe.Pointer // ICoreWebView2Environment*
	ready    bool
	fallback bool
}

var (
	stateMu sync.Mutex
	state   *appState
)

// Run 启动桌面窗口并阻塞直到窗口关闭。
func Run(opt Options) error {
	opt = opt.withDefaults()
	// 窗口与消息循环必须在同一个 OS 线程上，且该线程不能被动迁移。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 初始化 COM（STA —— 窗口与 WebView2 都要求）
	if hr, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded); hr != 0 && hr != 1 {
		// 1 = S_FALSE（已初始化过），不算失败
		log.Printf("WARN: CoInitializeEx 返回 0x%X（继续尝试）", hr)
	}

	hwnd, err := createWindow(opt)
	if err != nil {
		// 连窗口都建不起来 → 退到浏览器 App 模式（不再报错，因为已经给了可用路径）
		runBrowserApp(opt, fmt.Sprintf("创建窗口失败：%v", err))
		if opt.OnClose != nil {
			opt.OnClose()
		}
		return nil
	}

	st := &appState{hwnd: hwnd, opts: opt}
	stateMu.Lock()
	state = st
	stateMu.Unlock()

	// 先试 WebView2；失败则降级到浏览器 App 模式（窗口仍保留为提示）
	if !opt.PreferBrowserApp {
		if dll, lerr := loadWebView2(); lerr != nil {
			log.Printf("INFO: 未启用内嵌 WebView2（%v）", lerr)
			log.Printf("INFO: 提示：把 WebView2Loader.dll 放到本程序同目录即可启用内嵌窗口")
			st.fallback = true
		} else if werr := initWebView2(dll, st); werr != nil {
			log.Printf("WARN: WebView2 初始化失败（%v），改用浏览器 App 模式", werr)
			st.fallback = true
		}
	} else {
		st.fallback = true
	}

	if st.fallback {
		go func() {
			mode := runBrowserApp(opt, "")
			if opt.OnReady != nil {
				opt.OnReady(mode)
			}
		}()
	}

	procShowWindow.Call(uintptr(hwnd), swShow)
	procUpdateWindow.Call(uintptr(hwnd))

	// 消息循环
	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	if opt.OnClose != nil {
		opt.OnClose()
	}
	return nil
}

// createWindow 注册窗口类并创建窗口。
func createWindow(opt Options) (syscall.Handle, error) {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("Trae2ApiDesktopWindow")
	title, _ := syscall.UTF16PtrFromString(opt.Title)
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)

	wc := wndClassExW{
		cbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		lpfnWndProc:   syscall.NewCallback(wndProc),
		hInstance:     syscall.Handle(hInst),
		hCursor:       syscall.Handle(cursor),
		hbrBackground: 16, // COLOR_WINDOW+1：先用系统背景色，WebView2 起来后会盖住
		lpszClassName: className,
	}
	if ret, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		return 0, fmt.Errorf("RegisterClassExW: %v", err)
	}

	// 按客户区尺寸反推窗口尺寸（否则 WebView2 的可用区域会小于预期）
	r := rect{0, 0, int32(opt.Width), int32(opt.Height)}
	procAdjustWindowRect.Call(uintptr(unsafe.Pointer(&r)), wsOverlappedWindow, 0)
	w, h := r.Right-r.Left, r.Bottom-r.Top

	// 居中
	sw, _, _ := procGetSystemMetrics.Call(smCXScreen)
	sh, _, _ := procGetSystemMetrics.Call(smCYScreen)
	x := (int32(sw) - w) / 2
	y := (int32(sh) - h) / 2

	hwnd, _, err := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		wsOverlappedWindow,
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return 0, fmt.Errorf("CreateWindowExW: %v", err)
	}
	return syscall.Handle(hwnd), nil
}

// wndProc 是窗口过程。
//
// 注意签名必须是 syscall.NewCallback 要求的形式（全部 uintptr）。
func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	switch uint32(message) {
	case wmSize:
		// 窗口尺寸变化 → 让 WebView2 填满客户区。
		// 不处理的话 WebView2 会保持初始尺寸，缩放窗口时内容不跟随。
		resizeWebView()
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return ret
}

// resizeWebView 把 WebView2 的 bounds 设为当前客户区。
func resizeWebView() {
	stateMu.Lock()
	st := state
	stateMu.Unlock()
	if st == nil || st.ctrl == nil {
		return
	}
	var r rect
	procGetClientRect.Call(uintptr(st.hwnd), uintptr(unsafe.Pointer(&r)))
	// ICoreWebView2Controller::put_Bounds = vtable 索引 6
	comCall(st.ctrl, 6, uintptr(unsafe.Pointer(&r)))
}

// initWebView2 创建 WebView2 环境并挂到窗口上。
//
// 链路：CreateCoreWebView2EnvironmentWithOptions
//
//	→ handler: ICoreWebView2Environment::CreateCoreWebView2Controller
//	→ handler: ICoreWebView2Controller::get_CoreWebView2 → Navigate
func initWebView2(dll *syscall.LazyDLL, st *appState) error {
	createEnv := dll.NewProc("CreateCoreWebView2EnvironmentWithOptions")
	if err := createEnv.Find(); err != nil {
		return fmt.Errorf("找不到 CreateCoreWebView2EnvironmentWithOptions: %w", err)
	}

	// userDataFolder：WebView2 需要可写目录存放缓存。
	// 不放临时目录：临时目录被清理会导致每次启动都重建缓存。
	dataDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "Trae2Api", "WebView2")
	if dataDir == "" || dataDir == filepath.Join("", "Trae2Api", "WebView2") {
		dataDir = filepath.Join(os.TempDir(), "Trae2Api-WebView2")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("创建 WebView2 数据目录: %w", err)
	}
	dataPtr, _ := syscall.UTF16PtrFromString(dataDir)

	envHandler := newCOMHandler(func(hr, arg uintptr) {
		if int32(hr) < 0 || arg == 0 {
			log.Printf("WARN: 创建 WebView2 环境失败（HRESULT 0x%X）", hr)
			return
		}
		// ⚠️ 必须 AddRef：Invoke 传进来的指针是**借用**的，不 AddRef 的话
		// 回调返回后 WebView2 会释放环境 → 浏览器进程退出 → 窗口空白。
		// （这正是第一版的 bug：日志显示"就绪"，但 profile 建完进程就没了。）
		st.env = unsafe.Pointer(arg)
		comCall(st.env, 1) // AddRef
		// ICoreWebView2Environment::CreateCoreWebView2Controller = 索引 3
		ctrlHandler := newCOMHandler(func(hr2, arg2 uintptr) {
			if int32(hr2) < 0 || arg2 == 0 {
				log.Printf("WARN: 创建 WebView2 控制器失败（HRESULT 0x%X）", hr2)
				return
			}
			st.ctrl = unsafe.Pointer(arg2)
			comCall(st.ctrl, 1) // AddRef（同上）
			resizeWebView()     // put_Bounds = 索引 6（内部读客户区）
			// ICoreWebView2Controller::put_IsVisible(TRUE) = 索引 4
			if r := comCall(st.ctrl, 4, 1); int32(r) < 0 {
				log.Printf("WARN: put_IsVisible 失败（HRESULT 0x%X）", r)
			}
			// get_CoreWebView2 = 索引 25（返回的是我们拥有的引用，无需再 AddRef）
			var wv unsafe.Pointer
			if r := comCall(st.ctrl, 25, uintptr(unsafe.Pointer(&wv))); int32(r) < 0 || wv == nil {
				log.Printf("WARN: 取 CoreWebView2 失败（HRESULT 0x%X）", r)
				return
			}
			st.webview = wv
			urlPtr, _ := syscall.UTF16PtrFromString(st.opts.URL)
			// ICoreWebView2::Navigate = 索引 5
			if r := comCall(st.webview, 5, uintptr(unsafe.Pointer(urlPtr))); int32(r) < 0 {
				log.Printf("WARN: Navigate 失败（HRESULT 0x%X）", r)
				return
			}
			st.ready = true
			log.Printf("INFO: 桌面窗口已就绪（内嵌 WebView2），正在打开 %s", st.opts.URL)
			if st.opts.OnReady != nil {
				st.opts.OnReady(ModeWebView2)
			}
		})
		comCall(st.env, 3, uintptr(st.hwnd), ctrlHandler.ptr)
	})

	hr, _, _ := createEnv.Call(
		0, // browserExecutableFolder：0 = 用已安装的运行时
		uintptr(unsafe.Pointer(dataPtr)),
		0, // environmentOptions
		envHandler.ptr,
	)
	if int32(hr) < 0 {
		return fmt.Errorf("CreateCoreWebView2EnvironmentWithOptions 返回 0x%X（WebView2 运行时可能未安装）", hr)
	}
	return nil
}

// ---- 浏览器 App 模式降级 ----

// runBrowserApp 用 Edge/Chrome 的 --app 模式打开一个无地址栏窗口。
//
// 为什么把它作为降级而不是主方案：它是**独立进程**，关窗不会通知网关，
// 也无法从 Go 侧注入脚本/样式。但它零依赖、在任何装了 Edge/Chrome 的机器上都能用。
func runBrowserApp(opt Options, reason string) Mode {
	if reason != "" {
		log.Printf("INFO: 改用浏览器 App 模式（%s）", reason)
	}
	exe := findBrowser()
	if exe == "" {
		log.Printf("INFO: 未找到 Edge/Chrome，请手动打开 %s", opt.URL)
		if reason != "" {
			showMessage(opt.Title, fmt.Sprintf("%s\n\n未找到 Edge/Chrome，请手动打开：\n%s", reason, opt.URL))
		}
		return ModeNone
	}
	// --app=<url> 打开无地址栏、无标签页的窗口，外观接近原生应用。
	// --user-data-dir 用独立目录：避免与用户日常浏览器抢会话/扩展。
	profileDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "Trae2Api", "BrowserProfile")
	if profileDir == "" || strings.HasPrefix(profileDir, "Trae2Api") {
		profileDir = filepath.Join(os.TempDir(), "Trae2Api-BrowserProfile")
	}
	_ = os.MkdirAll(profileDir, 0o755)

	cmd := exec.Command(exe,
		"--app="+opt.URL,
		"--user-data-dir="+profileDir,
		"--no-first-run",
		"--no-default-browser-check",
	)
	if err := cmd.Start(); err != nil {
		log.Printf("WARN: 启动浏览器失败: %v；请手动打开 %s", err, opt.URL)
		return ModeNone
	}
	log.Printf("INFO: 已用浏览器 App 模式打开 %s（%s）", opt.URL, filepath.Base(exe))
	// 不 Wait：让它独立运行。用户关窗即可。
	go func() { _ = cmd.Wait() }()
	return ModeBrowserApp
}

// findBrowser 按优先级查找可用的 Chromium 系浏览器。
func findBrowser() string {
	var candidates []string
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		candidates = append(candidates,
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`))
	}
	if pf := os.Getenv("ProgramFiles(x86)"); pf != "" {
		candidates = append(candidates,
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`))
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		candidates = append(candidates,
			filepath.Join(la, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(la, `Google\Chrome\Application\chrome.exe`))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	// 最后试 PATH
	for _, name := range []string{"msedge.exe", "chrome.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// showMessage 弹一个系统消息框（只在完全降级时用）。
func showMessage(title, text string) {
	t, _ := syscall.UTF16PtrFromString(title)
	b, _ := syscall.UTF16PtrFromString(text)
	const mbIconinformation = 0x40
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(b)), uintptr(unsafe.Pointer(t)), mbIconinformation)
}

// Available 报告当前环境是否具备内嵌窗口条件（不实际创建窗口）。
func Available() (Mode, string) {
	if _, err := loadWebView2(); err != nil {
		if findBrowser() != "" {
			return ModeBrowserApp, "未找到 WebView2Loader.dll，将使用浏览器 App 模式"
		}
		return ModeNone, "未找到 WebView2Loader.dll，也未找到 Edge/Chrome"
	}
	return ModeWebView2, ""
}
