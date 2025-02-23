//go:build !noquic

package main

import (
	"fmt"

	"github.com/e1732a364fed/v2ray_simple/advLayer/quic"
	"github.com/manifoldco/promptui"
)

func init() {

	cliCmdList = append(cliCmdList, CliCmd{
		"调节hy手动挡", func() {
			var arr = []string{"加速", "减速", "当前状态", "讲解"}

			Select := promptui.Select{
				Label: "请选择",
				Items: arr,
			}

			for {
				i, result, err := Select.Run()

				if err != nil {
					fmt.Printf("Prompt failed %v\n", err)
					return
				}

				fmt.Printf("你选择了 %s\n", result)

				switch i {
				case 0:
					quic.TheCustomRate -= 0.1
					fmt.Printf("调好了!当前rate %f\n", quic.TheCustomRate)
				case 1:
					quic.TheCustomRate += 0.1
					fmt.Printf("调好了!当前rate %f\n", quic.TheCustomRate)
				case 2:
					fmt.Printf("当前rate %f\n", quic.TheCustomRate)
				case 3:
					fmt.Printf("rate越小越加速, rate越大越减速. 最小0.2最大1.5。实际速度倍率为 1.5/rate \n")
				}
			}
		},
	})

}
