package agent

import (
	"github.com/FuYue-WanJie/owltide/core/config"
	"github.com/FuYue-WanJie/owltide/core/decision"
	"github.com/FuYue-WanJie/owltide/core/model"
	"github.com/FuYue-WanJie/owltide/core/tool"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func agentOptions(cfg config.Config, c *model.Client, reg *tool.Registry, dl *decision.Logger) Options {
	return Options{
		Config:    cfg,
		Client:    c,
		Tools:     reg,
		Decisions: dl,
		SessionID: "test",
	}
}
