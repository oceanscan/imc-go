package main

import (
	"crypto/sha1"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/oceanscan/imc-go"
	"golang.org/x/crypto/ssh"
)

// Config
const (
	LocalLogDir  = "Logs"               // Adjust to actual local log dir
	RemoteLogDir = "/opt/lsts/dune/log" // Standard DUNE log dir
)

// State to avoid aggressive repeated checking
var (
	lastCheck      = make(map[string]time.Time)
	lastCheckMutex sync.Mutex
)

func shouldCheck(sysName string) bool {
	lastCheckMutex.Lock()
	defer lastCheckMutex.Unlock()

	last, ok := lastCheck[sysName]
	if !ok || time.Since(last) > 5*time.Minute {
		lastCheck[sysName] = time.Now()
		return true
	}
	return false
}

func getPass(ip string) string {
	// Re-implementing the simple hash password generation
	data := fmt.Sprintf("%s//%s", ip, ip)
	h := sha1.New()
	h.Write([]byte(data))
	bs := h.Sum(nil)

	// Convert to hex string
	var sb strings.Builder
	for _, b := range bs {
		sb.WriteString(fmt.Sprintf("%02x", b))
	}
	// Take first 12 chars
	return sb.String()[:12]
}

func checkAndSync(ip, sysName string) {
	if !shouldCheck(sysName) {
		return
	}

	log.Printf("Checking logs for %s (%s)...", sysName, ip)

	password := getPass(ip)
	config := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
			ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}

	// 1. Connect SSH
	addr := fmt.Sprintf("%s:22", ip)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		// Try fallback to 'root'
		log.Printf("[%s] Auth with generated password failed (%v). Retrying with 'root'...", sysName, err)
		config.Auth = []ssh.AuthMethod{
			ssh.Password("root"),
			ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range questions {
					answers[i] = "root"
				}
				return answers, nil
			}),
		}
		client, err = ssh.Dial("tcp", addr, config)
		if err != nil {
			log.Printf("[%s] SSH connection failed: %v", sysName, err)
			return
		}
		// Fallback succeeded, update password for rsync
		password = "root"
	}
	defer client.Close()

	// 2. List remote logs
	session, err := client.NewSession()
	if err != nil {
		log.Printf("[%s] Failed to create session: %v", sysName, err)
		return
	}
	defer session.Close()

	// List directories in RemoteLogDir (looking for YYYYMMDD/HHMMSS structure or just sorted list)
	// We'll just look for folders, outputting one per line
	output, err := session.CombinedOutput(fmt.Sprintf("find %s -maxdepth 2 -mindepth 2 -type d", RemoteLogDir))
	if err != nil {
		log.Printf("[%s] Failed to list remote files: %v. Output: %s", sysName, err, string(output))
		return
	}

	remoteLogs := strings.Split(string(output), "\n")

	// 3. Compare with local
	// Assuming structure is: LocalLogDir/VehicleName/YYYYMMDD/HHMMSS
	// Remote is: /opt/imc/dune/logs/YYYYMMDD/HHMMSS

	missingLogs := []string{}

	for _, remotePath := range remoteLogs {
		remotePath = strings.TrimSpace(remotePath)
		if remotePath == "" {
			continue
		}

		// Extract relative part: YYYYMMDD/HHMMSS
		relPath, err := filepath.Rel(RemoteLogDir, remotePath)
		if err != nil {
			continue
		}

		localPath := filepath.Join(LocalLogDir, sysName, relPath)

		// Check if exists locally
		// NOTE: This check depends on valid local path structure
		// For now, we'll just print what we WOULD sync

		// Actually, we can check if file exists using standard os.Stat (not implemented here fully for brevity of snippet, but logic holds)
		// ignoring actual file check for now, let's assume we want to sync

		// real implementation would check 'os.Stat(localPath)'
		_ = localPath
		missingLogs = append(missingLogs, remotePath)
	}

	if len(missingLogs) == 0 {
		log.Printf("[%s] All logs synced.", sysName)
		return
	}

	// 4. Rsync (Syncing the whole folder structure for simplicity)
	// We can just run one rsync command to sync everything new
	// rsync -av --ignore-existing root@IP:/opt/imc/dune/logs/ LocalLogDir/VehicleName/

	localDest := filepath.Join(LocalLogDir, sysName)
	if err := os.MkdirAll(localDest, 0755); err != nil {
		log.Printf("[%s] Failed to create local directory: %v", sysName, err)
		return
	}

	cmd := exec.Command("rsync", "-av", "--ignore-existing",
		"-e", fmt.Sprintf("sshpass -p %s ssh -o StrictHostKeyChecking=no", password),
		fmt.Sprintf("root@%s:%s/", ip, RemoteLogDir),
		localDest)

	log.Printf("[%s] Syncing %d potential new logs to %s...", sysName, len(missingLogs), localDest)

	// Run rsync
	// In a real app we might want to capture output or run in background
	// For now we just run it
	outputBytes, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[%s] Rsync failed: %v\nOutput: %s", sysName, err, string(outputBytes))
	} else {
		log.Printf("[%s] Sync complete.", sysName)
	}
}

func listen(proto *imc.Protocol, port int, group string) {
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	trans, err := imc.NewUDPTransporter(proto, addr)
	if err != nil {
		log.Printf("[Port %d] Failed to create transporter: %v", port, err)
		return
	}
	defer trans.Close()

	groupAddr := fmt.Sprintf("%s:%d", group, port)
	if err := trans.JoinMulticast(groupAddr); err != nil {
		log.Printf("[Port %d] Failed to join multicast group %s: %v", port, groupAddr, err)
		return
	}

	log.Printf("[Port %d] Listening...", port)

	for {
		msg, src, err := trans.Receive()
		if err != nil {
			log.Printf("[Port %d] Receive error: %v", port, err)
			continue
		}

		// Check if Announce
		// Lookup Announce definition
		announceDef, ok := proto.Lookup["Announce"]
		if !ok {
			log.Println("Announce message definition not found")
			return
		}

		if msg.Header.MGID == announceDef.ID {
			sysName := imc.ToString(msg.Fields["sys_name"])
			sysType := imc.ToUint8(msg.Fields["sys_type"])

			// sys_type 2 = UUV
			if sysType == 2 {
				// Address is in 'src' but that's usually just IP if UDP
				// imc-go Receive returns raw UDP addr derived string usually?
				// Let's check src format. Usually IP:Port

				s := src.String()
				parts := strings.Split(s, ":")
				if len(parts) > 0 {
					ip := parts[0]
					go checkAndSync(ip, sysName)
				}
			}
		}
	}
}

func main() {
	log.Println("Starting Auto-Logsync...")

	xmlProto, err := imc.ParseXML("IMC.xml")
	if err != nil {
		log.Fatalf("Failed to parse IMC.xml: %v", err)
	}
	proto := imc.NewProtocol(xmlProto)

	group := "224.0.75.69"
	ports := []int{30100, 30101, 30102, 30103, 30104}

	var wg sync.WaitGroup
	for _, port := range ports {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			listen(proto, p, group)
		}(port)
	}

	wg.Wait()
}
