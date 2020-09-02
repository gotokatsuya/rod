package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-rod/rod/lib/utils"
	"github.com/ysmood/lookpath"
)

var slash = filepath.FromSlash

func main() {
	lint()

	helper := get("helper.js")

	build := utils.S(`// generated by running "go generate" on project root

package assets

// Helper for rod
const Helper = {{.helper}}

// MousePointer for rod
const MousePointer = {{.mousePointer}}

// Monitor for rod
const Monitor = {{.monitor}}

// MonitorPage for rod
const MonitorPage = {{.monitorPage}}

// DeviceList for rod
const DeviceList = {{.deviceList}}
`,
		"helper", helper,
		"mousePointer", get("../../fixtures/mouse-pointer.svg"),
		"monitor", get("monitor.html"),
		"monitorPage", get("monitor-page.html"),
		"deviceList", getDeviceList(),
	)

	utils.E(utils.OutputFile(slash("lib/assets/assets.go"), build, nil))

	utils.E(utils.OutputFile(slash("lib/assets/js/main.go"), genHelperList(helper), nil))
}

func get(path string) string {
	code, err := utils.ReadString(slash("lib/assets/" + path))
	utils.E(err)
	return encode(code)
}

// not using encoding like base64 or gzip because of they will make git diff every large for small change
func encode(s string) string {
	s = strings.Replace(s, "// eslint-disable-next-line no-unused-expressions\n;", "", 1)
	return "`" + strings.ReplaceAll(s, "`", "` + \"`\" + `") + "`"
}

func lint() {
	cwd, _ := os.Getwd()
	_ = os.Chdir(slash("lib/assets"))
	defer func() { _ = os.Chdir(cwd) }()

	eslint, err := lookpath.LookPath(slash("node_modules/.bin"), "eslint")

	// install eslint if we don't have it
	if err != nil {
		utils.Exec("npm", "i", "--no-audit", "--no-fund")
		eslint, err = lookpath.LookPath(slash("node_modules/.bin"), "eslint")
		utils.E(err)
	}

	utils.Exec(eslint, "--fix", ".")
}

func getDeviceList() string {
	// we use the list from the web UI of devtools
	res, err := http.Get(
		"https://raw.githubusercontent.com/ChromeDevTools/devtools-frontend/master/front_end/emulated_devices/module.json",
	)
	utils.E(err)

	return encode(utils.MustReadJSON(res.Body).Get("extensions").Raw)
}

func genHelperList(helper string) string {
	m := regexp.MustCompile(`\},?\n\n {4}(?:async )?([a-z][^ ]+) \(`).FindAllStringSubmatch(helper, -1)
	list := "// generated by running \"go generate\" on project root\n\n" +
		"package js\n\n" +
		"// NameType type\n" +
		"type NameType string\n\n" +
		"const (\n"
	for _, g := range m {
		name := strings.ToUpper(g[1][:1]) + g[1][1:] + " NameType"
		list += fmt.Sprintf("\t//%s function name\n\t%s = \"%s\"\n", name, name, g[1])
	}
	list += ")\n"

	return list
}
