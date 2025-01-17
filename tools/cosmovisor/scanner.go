package cosmovisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cosmossdk.io/log"
	"cosmossdk.io/x/upgrade/plan"
	upgradetypes "cosmossdk.io/x/upgrade/types"
)

type fileWatcher struct {
	filename string // full path to a watched file
	interval time.Duration

	currentBin  string
	currentInfo upgradetypes.Plan
	lastModTime time.Time
	cancel      chan bool
	ticker      *time.Ticker

	needsUpdate   bool
	initialized   bool
	disableRecase bool
}

type callbackInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Repo    string `json:"repo"`
	Info    string `json:"info"`
	Height  int64  `json:"height"`
}

func newUpgradeFileWatcher(cfg *Config, logger log.Logger) (*fileWatcher, error) {
	filename := cfg.UpgradeInfoFilePath()
	if filename == "" {
		return nil, errors.New("filename undefined")
	}

	filenameAbs, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("invalid path: %s must be a valid file path: %w", filename, err)
	}

	dirname := filepath.Dir(filename)
	if info, err := os.Stat(dirname); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("invalid path: %s must be an existing directory: %w", dirname, err)
	}

	bin, err := cfg.CurrentBin()
	if err != nil {
		return nil, fmt.Errorf("error creating symlink to genesis: %w", err)
	}

	return &fileWatcher{
		currentBin:    bin,
		filename:      filenameAbs,
		interval:      cfg.PollInterval,
		currentInfo:   upgradetypes.Plan{},
		lastModTime:   time.Time{},
		cancel:        make(chan bool),
		ticker:        time.NewTicker(cfg.PollInterval),
		needsUpdate:   false,
		initialized:   false,
		disableRecase: cfg.DisableRecase,
	}, nil
}

func (fw *fileWatcher) Stop() {
	close(fw.cancel)
}

// MonitorUpdate pools the filesystem to check for new upgrade currentInfo.
// currentName is the name of currently running upgrade.  The check is rejected if it finds
// an upgrade with the same name.
func (fw *fileWatcher) MonitorUpdate(currentUpgrade upgradetypes.Plan) <-chan struct{} {
	fw.ticker.Reset(fw.interval)
	done := make(chan struct{})
	fw.cancel = make(chan bool)
	fw.needsUpdate = false

	go func() {
		for {
			select {
			case <-fw.ticker.C:
				if fw.CheckUpdate(currentUpgrade) {
					done <- struct{}{}
					return
				}

			case <-fw.cancel:
				return
			}
		}
	}()

	return done
}

// CheckUpdate reads update plan from file and checks if there is a new update request
// currentName is the name of currently running upgrade. The check is rejected if it finds
// an upgrade with the same name.
func (fw *fileWatcher) CheckUpdate(currentUpgrade upgradetypes.Plan) bool {
	if fw.needsUpdate {
		return true
	}

	stat, err := os.Stat(fw.filename)
	if err != nil {
		// file doesn't exists
		return false
	}

	if !stat.ModTime().After(fw.lastModTime) {
		return false
	}

	info, err := parseUpgradeInfoFile(fw.filename, fw.disableRecase)
	if err != nil {
		panic(fmt.Errorf("failed to parse upgrade info file: %w", err))
	}

	// extract version number and github url (if possible) for upnode deploy upgrade request
	version := ""
	repo := ""
	upgradeInfo, err := plan.ParseInfo(info.Info)
	if err == nil {
		for _, url := range upgradeInfo.Binaries {
			repo, version = getVersionAndRepoFromUrl(url)
			if version != "" {
				break
			}
		}
	}

	// callback even if no version number found, so the owner can at least be informed that an upgrade is expected
	callback := callbackInfo{
		Name:    info.Name,
		Version: version,
		Repo:    repo,
		Info:    info.Info,
		Height:  info.Height,
	}
	callbackJson, err := json.Marshal(callback)

	if err == nil {
		upgradeDetectedCallback(&callbackJson)
	}

	// file exist but too early in height
	currentHeight, _ := fw.checkHeight()
	if currentHeight != 0 && currentHeight < info.Height {
		return false
	}

	if !fw.initialized {
		// daemon has restarted
		fw.initialized = true
		fw.currentInfo = info
		fw.lastModTime = stat.ModTime()

		// Heuristic: Deamon has restarted, so we don't know if we successfully
		// downloaded the upgrade or not. So we try to compare the running upgrade
		// name (read from the cosmovisor file) with the upgrade info.
		if !strings.EqualFold(currentUpgrade.Name, fw.currentInfo.Name) {
			fw.needsUpdate = true
			upgradeHeightReachedCallback(&callbackJson)
			return true
		}
	}

	if info.Height > fw.currentInfo.Height {
		fw.currentInfo = info
		fw.lastModTime = stat.ModTime()
		fw.needsUpdate = true
		upgradeHeightReachedCallback(&callbackJson)
		return true
	}

	return false
}

func upgradeDetectedCallback(callbackJson *[]byte) {
	// report upgrade requirement back to upnode deploy
	callbackUrl := os.Getenv("CALLBACK_API") + "/internal/cosmos/" + os.Getenv("NODE_ID") + "/" + os.Getenv("DEPLOYMENT_ID") + "/cosmos_notify_upgrade"
	fmt.Println("upgrade callback to " + callbackUrl)
	http.Post(callbackUrl, "application/json", bytes.NewBuffer(*callbackJson))
}

func upgradeHeightReachedCallback(callbackJson *[]byte) {
	// send an alert to notify the backend that the upgrade height has been reached
	callbackUrl := os.Getenv("CALLBACK_API") + "/internal/cosmos/" + os.Getenv("NODE_ID") + "/" + os.Getenv("DEPLOYMENT_ID") + "/cosmos_upgrade_height_reached"
	fmt.Println("upgrade height callback to " + callbackUrl)
	http.Post(callbackUrl, "application/json", bytes.NewBuffer(*callbackJson))
}

func getVersionAndRepoFromUrl(url string) (string, string) {

	substrings := strings.Split(url, "/")
	githubIdx := -1
	ver := ""
	repo := ""
	for idx, str := range substrings {
		if strings.EqualFold(str, "github.com") {
			githubIdx = idx
		}
		if githubIdx < 0 || idx <= githubIdx+2 {
			if idx > 0 {
				repo += "/"
			}
			repo += str
		}
		match, e := regexp.MatchString(`^[vV]\d+\.\d+\.\d+`, str)
		if match && e == nil {
			ver = str
			break
		}
	}
	if githubIdx < 0 {
		repo = ""
	}
	return repo, ver
}

// checkHeight checks if the current block height
func (fw *fileWatcher) checkHeight() (int64, error) {
	// TODO(@julienrbrt) use `if !testing.Testing()` from Go 1.22
	// The tests from `process_test.go`, which run only on linux, are failing when using `autod` that is a bash script.
	// In production, the binary will always be an application with a status command, but in tests it isn't not.
	if strings.HasSuffix(os.Args[0], ".test") {
		return 0, nil
	}

	result, err := exec.Command(fw.currentBin, "status").Output() //nolint:gosec // we want to execute the status command
	if err != nil {
		return 0, err
	}

	type response struct {
		SyncInfo struct {
			LatestBlockHeight string `json:"latest_block_height"`
		} `json:"SyncInfo"`
	}

	var resp response
	if err := json.Unmarshal(result, &resp); err != nil {
		return 0, err
	}

	if resp.SyncInfo.LatestBlockHeight == "" {
		return 0, errors.New("latest block height is empty")
	}

	return strconv.ParseInt(resp.SyncInfo.LatestBlockHeight, 10, 64)
}

func parseUpgradeInfoFile(filename string, disableRecase bool) (upgradetypes.Plan, error) {
	f, err := os.ReadFile(filename)
	if err != nil {
		return upgradetypes.Plan{}, err
	}

	if len(f) == 0 {
		return upgradetypes.Plan{}, errors.New("empty upgrade-info.json")
	}

	var upgradePlan upgradetypes.Plan
	if err := json.Unmarshal(f, &upgradePlan); err != nil {
		return upgradetypes.Plan{}, err
	}

	// required values must be set
	if err := upgradePlan.ValidateBasic(); err != nil {
		return upgradetypes.Plan{}, fmt.Errorf("invalid upgrade-info.json content: %w, got: %v", err, upgradePlan)
	}

	// normalize name to prevent operator error in upgrade name case sensitivity errors.
	if !disableRecase {
		upgradePlan.Name = strings.ToLower(upgradePlan.Name)
	}

	return upgradePlan, err
}
