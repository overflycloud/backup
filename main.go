package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/hantbk/vtsbackup/config"
	"github.com/hantbk/vtsbackup/helper"
	"github.com/hantbk/vtsbackup/logger"
	"github.com/hantbk/vtsbackup/model"
	"github.com/hantbk/vtsbackup/scheduler"
	"github.com/hantbk/vtsbackup/storage"
	"github.com/hantbk/vtsbackup/web"
	"github.com/schollz/progressbar/v3"
	"github.com/sevlyar/go-daemon"
	"github.com/spf13/viper"
	"github.com/urfave/cli/v2"
)

const (
	usage = "Backup agent."
)

var (
	configFile string
	version    = "master"
	signal     = flag.String("s", "", `Send signal to the daemon:
  quit — graceful shutdown
  stop — fast shutdown
  reload — reloading the configuration file`)
)

func buildFlags(flags []cli.Flag) []cli.Flag {
	return append(flags, &cli.StringFlag{
		Name:        "config",
		Aliases:     []string{"c"},
		Usage:       "Special a config file",
		Destination: &configFile,
	})
}

func termHandler(sig os.Signal) error {
	logger.Info("Received QUIT signal, exiting...")
	scheduler.Stop()
	os.Exit(0)
	return nil
}

func reloadHandler(sig os.Signal) error {
	logger.Info("Reloading config...")
	err := config.Init(configFile)
	if err != nil {
		logger.Error(err)
	}

	return nil
}

func main() {

	app := cli.NewApp()

	app.Version = version
	app.Name = "vtsbackup"
	app.Usage = usage

	daemon.AddCommand(daemon.StringFlag(signal, "quit"), syscall.SIGQUIT, termHandler)
	daemon.AddCommand(daemon.StringFlag(signal, "stop"), syscall.SIGTERM, termHandler)
	daemon.AddCommand(daemon.StringFlag(signal, "reload"), syscall.SIGHUP, reloadHandler)

	app.Commands = []*cli.Command{
		{
			Name:  "perform",
			Usage: "Perform backup pipeline using config file",
			Flags: buildFlags([]cli.Flag{
				&cli.StringSliceFlag{
					Name:    "model",
					Aliases: []string{"m"},
					Usage:   "Model name that you want perform",
				},
			}),
			Action: func(ctx *cli.Context) error {
				var modelNames []string
				err := initApplication()
				if err != nil {
					return err
				}
				modelNames = append(ctx.StringSlice("model"), ctx.Args().Slice()...)
				return perform(modelNames)
			},
		},
		{
			Name:  "start",
			Usage: "Start Backup agent as daemon",
			Flags: buildFlags([]cli.Flag{}),
			Action: func(ctx *cli.Context) error {
				fmt.Println("Backup starting as daemon...")

				args := []string{"vtsbackup", "run"}
				if len(configFile) != 0 {
					args = append(args, "--config", configFile)
				}

				dm := &daemon.Context{
					PidFileName: config.PidFilePath,
					PidFilePerm: 0644,
					WorkDir:     "./",
					Args:        args,
				}

				d, err := dm.Reborn()
				if err != nil {
					return fmt.Errorf("start failed, please check is there another instance running: %w", err)
				}

				if d != nil {
					return nil
				}
				defer dm.Release() //nolint:errcheck

				logger.SetLogger(config.LogFilePath)

				err = initApplication()
				if err != nil {
					return err
				}

				if err := scheduler.Start(); err != nil {
					return fmt.Errorf("failed to start scheduler: %w", err)
				}

				return nil
			},
		},
		{
			Name:  "run",
			Usage: "Run Backup agent without daemon",
			Flags: buildFlags([]cli.Flag{}),
			Action: func(ctx *cli.Context) error {
				logger.SetLogger(config.LogFilePath)

				err := initApplication()
				if err != nil {
					return err
				}

				if err := scheduler.Start(); err != nil {
					return fmt.Errorf("failed to start scheduler: %w", err)
				}

				return web.StartHTTP(version)
			},
		},
		// {
		// 	Name:  "list-agent",
		// 	Usage: "List running Backup agents",
		// 	Action: func(ctx *cli.Context) error {
		// 		pids, err := listBackupAgents()
		// 		if err != nil {
		// 			return err
		// 		}
		// 		if len(pids) == 0 {
		// 			fmt.Println("No running Backup agents found.")
		// 		} else {
		// 			fmt.Println("Running Backup agents PIDs:")
		// 			for _, pid := range pids {
		// 				fmt.Println(pid)
		// 			}
		// 		}
		// 		return nil
		// 	},
		// },
		{
			Name:  "stop-agent",
			Usage: "Stop the running Backup agent",
			Action: func(ctx *cli.Context) error {
				fmt.Println("Stopping Backup agent...")
				pids, err := listBackupAgents()
				if err != nil {
					return err
				}
				if len(pids) == 0 {
					fmt.Println("No running Backup agents found.")
				} else {
					fmt.Println("Running Backup agents PIDs:")
					for _, pid := range pids {
						fmt.Println(pid)
					}
				}
				for _, pid := range pids {
					stopBackupAgent(pid)
				}
				return nil
			},
		},
		{
			Name:  "reload-agent",
			Usage: "Reload the running Backup agent",
			Action: func(ctx *cli.Context) error {
				fmt.Println("Reloading Backup agent...")
				pids, err := listBackupAgents()
				if err != nil {
					return err
				}

				if len(pids) == 0 {
					fmt.Println("No running Backup agents found.")
				} else {
					fmt.Println("Running Backup agents PIDs:")
					for _, pid := range pids {
						fmt.Println(pid)
					}
				}
				for _, pid := range pids {
					reloadBackupAgent(pid)
				}
				return nil
			},
		},
		{
			Name:  "list-model",
			Usage: "List all configured backup models",
			Flags: buildFlags([]cli.Flag{}),
			Action: func(ctx *cli.Context) error {
				return listModel()
			},
		},
		{
			Name:  "list-backup",
			Usage: "List backup files for a specific model",
			Flags: buildFlags([]cli.Flag{
				&cli.StringFlag{
					Name:     "model",
					Aliases:  []string{"m"},
					Usage:    "Model name to list backups for",
					Required: true,
				},
			}),
			Action: func(ctx *cli.Context) error {
				modelName := ctx.String("model")
				return listBackupFiles(modelName)
			},
		},
		{
			Name:  "download-backup",
			Usage: "Download a backup file for a specific model",
			Flags: buildFlags([]cli.Flag{
				&cli.StringFlag{
					Name:     "model",
					Aliases:  []string{"m"},
					Usage:    "Model name to download backup from",
					Required: true,
				},
				&cli.StringFlag{
					Name:     "output",
					Aliases:  []string{"o"},
					Usage:    "Path to save backup file",
					Required: true,
				},
			}),
			Action: func(ctx *cli.Context) error {
				modelName := ctx.String("model")
				outputPath := ctx.String("output")
				return downloadBackupFile(modelName, outputPath)
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		logger.Fatal(err.Error())
	}
}

func initApplication() error {
	return config.Init(configFile)
}

func perform(modelNames []string) error {
	var models []*model.Model
	if len(modelNames) == 0 {
		// perform all
		models = model.GetModels()
	} else {
		for _, name := range modelNames {
			if m := model.GetModelByName(name); m == nil {
				return fmt.Errorf("model %s not found in %s", name, viper.ConfigFileUsed())
			} else {
				models = append(models, m)
			}
		}
	}

	for _, m := range models {
		if err := m.Perform(); err != nil {
			logger.Tag(fmt.Sprintf("Model %s", m.Config.Name)).Error(err)
		}
	}

	return nil
}

func listBackupAgents() ([]int, error) {
	cmd := exec.Command("ps", "aux")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("error: %v", err)
	}

	var pids []int
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "vtsbackup") && !strings.Contains(line, "grep") {
			fields := strings.Fields(line)
			if len(fields) > 1 {
				pid, err := strconv.Atoi(fields[1])
				if err == nil {
					pids = append(pids, pid)
				}
			}
		}
	}
	return pids, nil
}

func stopBackupAgent(pid int) {
	cmd := exec.Command("kill", "-QUIT", strconv.Itoa(pid))
	err := cmd.Run()
	if err != nil {
		fmt.Println("Error:", err)
	} else {
		fmt.Println("Backup agent stopped successfully")
	}
}

func reloadBackupAgent(pid int) {
	cmd := exec.Command("kill", "-HUP", strconv.Itoa(pid))
	err := cmd.Run()
	if err != nil {
		fmt.Println("Error:", err)
	} else {
		fmt.Println("Backup agent reloaded successfully")
	}
}

func listModel() error {
	err := initApplication()
	if err != nil {
		return err
	}
	models := model.GetModels()
	if len(models) == 0 {
		fmt.Println("No backup models configured.")
	} else {
		fmt.Printf("Configured backup models (from %s):\n", viper.ConfigFileUsed())
		for _, m := range models {
			fmt.Printf("- %s\n", m.Config.Name)
			if m.Config.Description != "" {
				fmt.Printf("  Description: %s\n", m.Config.Description)
			}
			if m.Config.Schedule.Enabled {
				fmt.Printf("  Schedule: %s\n", m.Config.Schedule.String())
			}
			fmt.Println()
		}
	}
	return nil
}

func listBackupFiles(modelName string) error {
	err := initApplication()
	if err != nil {
		return err
	}

	m := model.GetModelByName(modelName)
	if m == nil {
		return fmt.Errorf("model: %q not found", modelName)
	}

	files, err := storage.List(m.Config, "/")
	if err != nil {
		return fmt.Errorf("failed to list backup files: %v", err)
	}

	if len(files) == 0 {
		fmt.Printf("No backup files found for model %q\n", modelName)
	} else {
		fmt.Printf("Backup files for model %q:\n", modelName)
		for _, file := range files {
			fmt.Printf("- %s (Size: %s, Last Modified: %s)\n",
				file.Filename,
				humanize.Bytes(uint64(file.Size)),
				file.LastModified.Format(time.RFC3339),
			)
		}
	}

	return nil
}

func downloadBackupFile(modelName, outputPath string) error {
	if outputPath == "" {
		outputPath = "."
	}
	err := initApplication()
	if err != nil {
		return err
	}

	m := model.GetModelByName(modelName)
	if m == nil {
		return fmt.Errorf("model: %q not found", modelName)
	}

	files, err := storage.List(m.Config, "/")
	if err != nil {
		return fmt.Errorf("failed to list backup files: %v", err)
	}

	if len(files) == 0 {
		fmt.Printf("No backup files found for model %q\n", modelName)
		return nil
	}

	fmt.Printf("Backup files for model %q:\n", modelName)
	for i, file := range files {
		fmt.Printf("%d. %s (Size: %s, Last Modified: %s)\n",
			i+1,
			file.Filename,
			humanize.Bytes(uint64(file.Size)),
			file.LastModified.Format(time.RFC3339),
		)
	}

	var choice int
	fmt.Print("Enter the number of the file you want to download (0 to cancel): ")
	_, err = fmt.Scanf("%d", &choice)
	if err != nil || choice < 0 || choice > len(files) {
		return fmt.Errorf("invalid choice")
	}

	if choice == 0 {
		fmt.Println("Download cancelled.")
		return nil
	}

	selectedFile := files[choice-1]

	fmt.Printf("You selected: %s\n", selectedFile.Filename)
	fmt.Print("Do you want to proceed with the download? (Y/n): ")
	var confirm string
	fmt.Scanf("%s", &confirm)

	if strings.ToLower(confirm) != "y" && confirm != "" {
		fmt.Println("Download cancelled.")
		return nil
	}

	downloadURL, err := storage.Download(m.Config, selectedFile.Filename)
	if err != nil {
		return fmt.Errorf("failed to get download URL: %v", err)
	}

	filePath := filepath.Join(outputPath, selectedFile.Filename)
	dirPath := filepath.Dir(filePath)

	// Ensure the output directory exists
	if err := helper.MkdirP(dirPath); err != nil {
		return fmt.Errorf("failed to create output directory: %v", err)
	}

	fmt.Printf("Downloading %s to %s...\n", selectedFile.Filename, filePath)

	// Create the file
	out, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create file: %v", err)
	}
	defer out.Close()

	// Get the data
	resp, err := http.Get(downloadURL)
	if err != nil {
		return fmt.Errorf("failed to download file: %v", err)
	}
	defer resp.Body.Close()

	// Create a progress bar
	bar := progressbar.DefaultBytes(
		resp.ContentLength,
		"Downloading",
	)

	// Write the body to file
	_, err = io.Copy(io.MultiWriter(out, bar), resp.Body)
	if err != nil {
		return fmt.Errorf("failed to save file: %v", err)
	}

	fmt.Printf("\nFile downloaded successfully: %s\n", filePath)
	return nil
}
