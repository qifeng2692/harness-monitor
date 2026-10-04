// Command harness-monitor 是 harness-monitor 的 GUI 入口（Wails v2 + WebView2）。
//
// 桌面应用形态：左侧 harness/会话导航，右侧会话时间线（步骤内容全文可展开），
// 顶部工具栏（采集 / 刷新 / 搜索 / live 跟随）。
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:     "harness-monitor",
		Width:     1440,
		Height:    920,
		MinWidth:  1000,
		MinHeight: 640,
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup: app.startup,
		OnShutdown: app.shutdown,
		Bind: []interface{}{
			app,
		},
		Windows: &windows.Options{
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
	if err != nil {
		log.Fatal("启动失败: ", err)
	}
}
