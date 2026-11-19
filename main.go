package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	mode := flag.String("mode", "test", "test: 跑验收场景; web: 启动可视化页面")
	addr := flag.String("addr", ":8080", "web 模式监听地址")
	flag.Parse()

	switch *mode {
	case "test":
		reports, ok := RunScenarios()
		for _, rep := range reports {
			status := "PASS"
			if len(rep.Errors) > 0 {
				status = "FAIL"
			}
			fmt.Printf("== [%s] %s ==\n", status, rep.Name)
			for _, l := range rep.Lines {
				fmt.Println("  " + l)
			}
			for _, e := range rep.Errors {
				fmt.Println("  !! " + e)
			}
		}
		if !ok {
			os.Exit(1)
		}
	case "web":
		fmt.Println("仿真页面: http://localhost" + *addr)
		ServeWeb(*addr)
	}
}
