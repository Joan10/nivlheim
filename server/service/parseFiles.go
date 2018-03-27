package main

// Create tasks to parse new files that have been read into the database
import (
	"database/sql"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/lib/pq"
)

type parseFilesJob struct{}

func init() {
	RegisterJob(parseFilesJob{})
}

func (s parseFilesJob) HowOften() time.Duration {
	return time.Second * 3
}

func (s parseFilesJob) Run(db *sql.DB) {
	rows, err := db.Query("SELECT fileid FROM files WHERE parsed = false")
	if err != nil {
		log.Panic(err)
	}
	defer rows.Close()
	concurrent := make(chan bool, 8)
	for rows.Next() {
		var fileid sql.NullInt64
		rows.Scan(&fileid)
		if fileid.Valid {
			/*taskurl := "http://localhost/cgi-bin/parsefile?fileid=" +
				strconv.FormatInt(fileid.Int64, 10)
			if postgresSupportsOnConflict {
				_, err := db.Exec("INSERT INTO tasks(url) VALUES($1)"+
					" ON CONFLICT DO NOTHING", taskurl)
				if err != nil {
					log.Println(err.Error())
				}
			} else {
				db.Exec("INSERT INTO tasks(url) VALUES($1)", taskurl)
			}*/
			concurrent <- true
			go func() {
				defer func() { <-concurrent }()
				parseFile(db, int(fileid.Int64))
			}()
		}
	}
}

func parseFile(database *sql.DB, fileId int) {
	tx, err := database.Begin()
	if err != nil {
		log.Println(err)
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Println(r)
			tx.Rollback()
		} else if err != nil {
			log.Println(err)
			tx.Rollback()
		} else {
			tx.Exec("UPDATE files SET parsed = true WHERE fileid = $1", fileId)
			tx.Commit()
		}
	}()
	var filename, content, certcn, ipaddr, certfp, cVersion,
		osHostname sql.NullString
	var received pq.NullTime
	var isCommand sql.NullBool
	err = tx.QueryRow("SELECT filename, content, received, is_command, certcn,"+
		"ipaddr, certfp, clientversion, os_hostname FROM files WHERE fileid=$1",
		fileId).
		Scan(&filename, &content, &received, &isCommand, &certcn, &ipaddr,
			&certfp, &cVersion, &osHostname)
	if err != nil {
		return
	}
	if !certfp.Valid {
		panic(fmt.Sprintf("certfp is null for file %d", fileId))
	}
	// first, try to update as if there is an existing row
	result, err := tx.Exec("UPDATE hostinfo SET lastseen=$1,clientversion=$2 "+
		"WHERE certfp=$3", received, cVersion, certfp.String)
	if err != nil {
		return
	}
	rowcount, err := result.RowsAffected()
	if err != nil {
		return
	}
	if rowcount == 0 {
		// no existing row? then try to insert
		_, err = tx.Exec("INSERT INTO hostinfo(lastseen,ipaddr,clientversion,"+
			"os_hostname,certfp) VALUES($1,$2,$3,$4,$5)",
			received, ipaddr, cVersion, osHostname, certfp)
		if err != nil {
			// race condition (duplicate key value) or other error.
			return
		}
	} else {
		// The row exists already. This statement will set dnsttl to null
		// only if ipaddr or os_hostname changed.
		_, err = tx.Exec("UPDATE hostinfo SET ipaddr=$1, os_hostname=$2, "+
			"dnsttl=null WHERE (ipaddr!=$1 OR os_hostname!=$2) AND certfp=$3",
			ipaddr, osHostname, certfp)
		if err != nil {
			return
		}
	}
	tx.Commit()

	if filename.String == "/etc/redhat-release" {
		var os, osEdition string
		rhel := regexp.MustCompile("^Red Hat Enterprise Linux (\\w+)" +
			".*(Tikanga|Santiago|Maipo)")
		m := rhel.FindStringSubmatch(content.String)
		if m != nil {
			osEdition = m[1]
			switch m[2] {
			case "Tikanga":
				os = "RHEL 5"
			case "Santiago":
				os = "RHEL 6"
			case "Maipo":
				os = "RHEL 7"
			}
		} else {
			fedora := regexp.MustCompile("^Fedora release (\\d+)")
			m = fedora.FindStringSubmatch(content.String)
			if m != nil {
				os = "Fedora " + m[1]
			} else {
				centos := regexp.MustCompile("^CentOS Linux release (\\d+)")
				m = centos.FindStringSubmatch(content.String)
				if m != nil {
					os = "CentOS " + m[1]
				}
			}
		}
		if os != "" && osEdition != "" {
			_, err = tx.Exec("UPDATE hostinfo SET os=$1, os_edition=$2 WHERE certfp=$3",
				os, osEdition, certfp.String)
		} else if os != "" {
			_, err = tx.Exec("UPDATE hostinfo SET os=$1 WHERE certfp=$2",
				os, certfp.String)
		}
		return
	}

	edition := regexp.MustCompile("/usr/lib/os.release.d/os-release-([a-z]+)")
	if m := edition.FindStringSubmatch(filename.String); m != nil {
		_, err = tx.Exec("UPDATE hostinfo SET os_edition=$1 WHERE certfp=$2",
			strings.Title(m[1]), certfp.String)
		return
	}

	if filename.String == "/usr/bin/dpkg-query -l" {
		ubuntuEdition := regexp.MustCompile("ubuntu-(desktop|server)")
		if m := ubuntuEdition.FindStringSubmatch(content.String); m != nil {
			_, err = tx.Exec("UPDATE hostinfo SET os_edition=$1 WHERE certfp=$2",
				strings.Title(m[1]), certfp.String)
		}
		return
	}

	if filename.String == "/etc/debian_version" {
		re := regexp.MustCompile("^(\\d+)\\.")
		if m := re.FindStringSubmatch(content.String); m != nil {
			_, err = tx.Exec("UPDATE hostinfo SET os=$1 WHERE certfp=$2",
				"Debian "+m[1], certfp.String)
		}
		return
	}

	if filename.String == "/etc/lsb-release" {
		re := regexp.MustCompile(`DISTRIB_ID=Ubuntu(?s:.*)DISTRIB_RELEASE=(\d+)\.(\d+)`)
		if m := re.FindStringSubmatch(content.String); m != nil {
			_, err = tx.Exec("UPDATE hostinfo SET os=$1 WHERE certfp=$2",
				fmt.Sprintf("Ubuntu %d.%d", m[2], m[3]), certfp.String)
		}
		return
	}

	if filename.String == "/usr/bin/sw_vers" {
		re := regexp.MustCompile(`ProductName:\s+Mac OS X\nProductVersion:\s+(\d+\.\d+)`)
		if m := re.FindStringSubmatch(content.String); m != nil {
			_, err = tx.Exec("UPDATE hostinfo SET os=$1, os_edition=null "+
				"WHERE certfp=$2", "macOS "+m[1], certfp.String)
		}
		return
	}

	if filename.String == "(Get-WmiObject Win32_OperatingSystem).Caption" {
		reWinX := regexp.MustCompile(`Microsoft Windows (\d+)`)
		reWinServer := regexp.MustCompile(`Microsoft Windows Server (\d+)( R2)?`)
		if m := reWinX.FindStringSubmatch(content.String); m != nil {
			_, err = tx.Exec("UPDATE hostinfo SET os=$1, os_edition=null "+
				"WHERE certfp=$2", "Windows "+m[1], certfp.String)
		} else if m := reWinServer.FindStringSubmatch(content.String); m != nil {
			_, err = tx.Exec("UPDATE hostinfo SET os=$1, os_edition='Server' "+
				"WHERE certfp=$2", fmt.Sprintf("Windows %d%s", m[1], m[2]),
				certfp.String)
		}
		return
	}

	if filename.String == "/bin/uname -a" {
		re := regexp.MustCompile(`(\S+) \S+ (\S+)`)
		if m := re.FindStringSubmatch(content.String); m != nil {
			os := m[1]
			kernel := m[2]
			if os == "FreeBSD" {
				m = regexp.MustCompile(`^(\d+)`).FindStringSubmatch(kernel)
				if m != nil {
					os = "FreeBSD " + m[1]
				}
				_, err = tx.Exec("UPDATE hostinfo SET os=$1, os_edition=null, "+
					"kernel=$2 WHERE certfp=$3", os, kernel, certfp.String)
			} else {
				_, err = tx.Exec("UPDATE hostinfo SET kernel=$1 "+
					"WHERE certfp=$2", kernel, certfp.String)
			}
		}
		return
	}

	if filename.String == "/usr/sbin/dmidecode -t system" {
		re := regexp.MustCompile(`^System Information\n(?ms:.*?)^$`)
		if m := re.FindStringSubmatch(content.String); m != nil {
			info := m[1]
			var vendor, model, serial string
			if m = regexp.MustCompile(`Manufacturer: (.*?)\s*$`).
				FindStringSubmatch(info); m != nil {
				vendor = m[1]
				vendor = strings.Replace(vendor, "HP", "Hewlett-Packard", 1)
				vendor = strings.Replace(vendor, "HITACHI", "Hitachi", 1)
			}
			if m = regexp.MustCompile(`Product Name: (.*?)\s*$`).
				FindStringSubmatch(info); m != nil {
				model = m[1]
			}
			if m = regexp.MustCompile(`Serial Number: ([^\s]+)\s*$`).
				FindStringSubmatch(info); m != nil {
				serial = m[1]
			}
			_, err = tx.Exec("UPDATE hostinfo SET vendor=$1,model=$2,serialno=$3"+
				"WHERE certfp=$4", vendor, model, serial, certfp.String)
		}
		return
	}

	if filename.String == "[System.Environment]::OSVersion|ConvertTo-Json" {
		//m := make(map[string]interface{})
		//err = json.Unmarshal(([]byte)content.String, m)
	}

	if filename.String == "Get-WmiObject Win32_computersystemproduct|Select Name,Vendor|ConvertTo-Json" {
	}
}
