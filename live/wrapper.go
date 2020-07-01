package live

import (
	"github.com/fzxiao233/Vtb_Record/live/interfaces"
	"github.com/fzxiao233/Vtb_Record/live/monitor"
	"github.com/fzxiao233/Vtb_Record/live/plugins"
	"github.com/fzxiao233/Vtb_Record/live/videoworker"
	"github.com/fzxiao233/Vtb_Record/utils"
)

func StartMonitor(mon monitor.VideoMonitor, usersConfig utils.UsersConfig) {
	//ticker := time.NewTicker(time.Second * time.Duration(utils.Config.CheckSec))
	//for {
	pm := videoworker.PluginManager{}
	pm.AddPlugin(&plugins.PluginCQBot{})
	pm.AddPlugin(&plugins.PluginTranslationRecorder{})
	pm.AddPlugin(&plugins.PluginUploader{})

	var fun = func(mon monitor.VideoMonitor) *interfaces.LiveStatus {
		return &interfaces.LiveStatus{
			IsLive: mon.CheckLive(usersConfig),
			Video:  mon.CreateVideo(usersConfig),
		}
	}

	videoworker.StartProcessVideo(fun, mon, pm)
	return
	//<-ticker.C
	//}
}
