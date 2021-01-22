package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
)

var (
	debugflag  = flag.Bool("debug", true, "enable debug output")
	configflag = flag.String("config", "", "configuration file")

	defaultPATH = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
)

func exit() {
	if err := recover(); err != nil {
		if _, ok := err.(runtime.Error); ok {
			log.Println(err)
		}
		if *debugflag {
			log.Printf("%s", debug.Stack())
		}
		os.Exit(1)
	}
	os.Exit(0)
}

func getPATH(config CLIConfig) string {
	dirs := filepath.SplitList(os.Getenv("PATH"))
	// directories from the hook environment have higher precedence
	dirs = append(dirs, defaultPATH...)

	if config.Root != nil {
		rootDirs := []string{}
		for _, dir := range dirs {
			rootDirs = append(rootDirs, path.Join(*config.Root, dir))
		}
		// directories with the root prefix have higher precedence
		dirs = append(rootDirs, dirs...)
	}
	return strings.Join(dirs, ":")
}

// 从系统PATH中获取 nvidia-container-cli 二进制文件的路径，该组件是libnvidia-container的命令行
func getCLIPath(config CLIConfig) string {
	if config.Path != nil {
		return *config.Path
	}

	if err := os.Setenv("PATH", getPATH(config)); err != nil {
		log.Panicln("couldn't set PATH variable:", err)
	}

	path, err := exec.LookPath("nvidia-container-cli")
	if err != nil {
		log.Panicln("couldn't find binary nvidia-container-cli in", os.Getenv("PATH"), ":", err)
	}
	return path
}

// getRootfsPath returns an absolute path. We don't need to resolve symlinks for now.
func getRootfsPath(config containerConfig) string {
	rootfs, err := filepath.Abs(config.Rootfs)
	if err != nil {
		log.Panicln(err)
	}
	return rootfs
}

func doPrestart() {
	var err error

	defer exit()
	log.SetFlags(0)

	hook := getHookConfig()
	cli := hook.NvidiaContainerCLI

	//查询容器的配置参数
	container := getContainerConfig(hook)
	//获取GPU相关的配置参数
	nvidia := container.Nvidia
	if nvidia == nil {
		// Not a GPU container, nothing to do.
		return
	}

	rootfs := getRootfsPath(container)

	//获取 nvidia-container-cli 的安装路径，将路径放在[]string{}切片args中
	args := []string{getCLIPath(cli)}
	//使用该命令进行容器的GPU相关配置，下面的全都是为这个 cli 构造参数
	//root权限
	if cli.Root != nil {
		args = append(args, fmt.Sprintf("--root=%s", *cli.Root))
	}
	//--load-kmods 参数
	if cli.LoadKmods {
		args = append(args, "--load-kmods")
	}
	if cli.NoPivot {
		args = append(args, "--no-pivot")
	}
	if *debugflag {
		args = append(args, "--debug=/dev/stderr")
	} else if cli.Debug != nil {
		args = append(args, fmt.Sprintf("--debug=%s", *cli.Debug))
	}
	if cli.Ldcache != nil {
		args = append(args, fmt.Sprintf("--ldcache=%s", *cli.Ldcache))
	}
	if cli.User != nil {
		args = append(args, fmt.Sprintf("--user=%s", *cli.User))
	}
	args = append(args, "configure")

	if cli.Ldconfig != nil {
		args = append(args, fmt.Sprintf("--ldconfig=%s", *cli.Ldconfig))
	}
	if cli.NoCgroups {
		args = append(args, "--no-cgroups")
	}
	//将设置的GPU 环境变量或者挂载转变为device
	if len(nvidia.Devices) > 0 {
		log.Println("nvidia.Devices:",nvidia.Devices)
		log.Println("args for cli:",args)
		//time.Sleep(1*time.Minute)
		//nvidia.Devices = "GPU-3c31cd14-a562-c0d4-5f1f-dce6374e4577"
		//nvidia.Devices = "1"
		file,_ := os.OpenFile("/usr/bin/gpu.config",os.O_RDWR|os.O_CREATE,0755)
		data,_ := ioutil.ReadAll(file)
		gpus := string(data)
		gpus = strings.Replace(gpus, "\n", "", -1)
		nvidia.Devices = gpus
		args = append(args, fmt.Sprintf("--device=%s", nvidia.Devices))
	}
	//mig 配置
	if len(nvidia.MigConfigDevices) > 0 {
		args = append(args, fmt.Sprintf("--mig-config=%s", nvidia.MigConfigDevices))
	}
	if len(nvidia.MigMonitorDevices) > 0 {
		args = append(args, fmt.Sprintf("--mig-monitor=%s", nvidia.MigMonitorDevices))
	}

	for _, cap := range strings.Split(nvidia.DriverCapabilities, ",") {
		if len(cap) == 0 {
			break
		}
		args = append(args, capabilityToCLI(cap))
	}

	if !hook.DisableRequire && !nvidia.DisableRequire {
		for _, req := range nvidia.Requirements {
			args = append(args, fmt.Sprintf("--require=%s", req))
		}
	}

	args = append(args, fmt.Sprintf("--pid=%s", strconv.FormatUint(uint64(container.Pid), 10)))
	args = append(args, rootfs)

	//至此，参数构建完毕
	//获取原有环境变量
	//os.Setenv("NVIDIA_VISIBLE_DEVICES","1")
	env := append(os.Environ(), cli.Environment...)
	//args[0]为nvidia-container-cli的路径，相当于执行该命令，在参数args、env下
	///usr/bin/nvidia-container-cli  --load-kmods  --debug=/var/log/nvidia-container-toolkit.log  configure --ldconfig=@/sbin/ldconfig --device=all --compute --utility  --pid=78717  /var/lib/docker/overlay2/6ac97e95475e9df0f32f7e2f7251ca053651c62292d1a5127c71d33e55904d2b/merged
	err = syscall.Exec(args[0], args, env)
	log.Panicln("exec failed:", err)
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
	flag.PrintDefaults()
	fmt.Fprintf(os.Stderr, "\nCommands:\n")
	fmt.Fprintf(os.Stderr, "  prestart\n        run the prestart hook\n")
	fmt.Fprintf(os.Stderr, "  poststart\n        no-op\n")
	fmt.Fprintf(os.Stderr, "  poststop\n        no-op\n")
}

func main() {
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}

	// 走 prestart 流程
	switch args[0] {
	case "prestart":
		doPrestart()
		os.Exit(0)
	case "poststart":
		fallthrough
	case "poststop":
		os.Exit(0)
	default:
		flag.Usage()
		os.Exit(2)
	}
}
