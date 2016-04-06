// nmon2influxdb
// import nmon report in InfluxDB
// author: adejoux@djouxtech.net

package main

import (
	"bufio"
	"fmt"
	"github.com/adejoux/influxdbclient"
	"github.com/codegangsta/cli"
	"io/ioutil"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var hostRegexp = regexp.MustCompile(`^AAA,host,(\S+)`)
var osRegexp = regexp.MustCompile(`^AAA,.*(Linux|AIX)`)
var timeRegexp = regexp.MustCompile(`^ZZZZ,([^,]+),(.*)$`)
var intervalRegexp = regexp.MustCompile(`^AAA,interval,(\d+)`)
var headerRegexp = regexp.MustCompile(`^AAA|^BBB|^UARG|,T\d`)
var infoRegexp = regexp.MustCompile(`AAA,(.*)`)
var badRegexp = regexp.MustCompile(`,,`)
var cpuallRegexp = regexp.MustCompile(`^CPU\d+|^SCPU\d+|^PCPU\d+`)
var diskallRegexp = regexp.MustCompile(`^DISK`)
var skipRegexp = regexp.MustCompile(`T0+,|^Z|^TOP,%CPU`)
var statsRegexp = regexp.MustCompile(`^[^,]+?,(T\d+)`)
var topRegexp = regexp.MustCompile(`^TOP,\d+,(T\d+)`)
var nfsRegexp = regexp.MustCompile(`^NFS`)
var nameRegexp = regexp.MustCompile(`(\d+)$`)

func NmonImport(c *cli.Context) {

	if len(c.Args()) < 1 {
		fmt.Printf("file name or directory needs to be provided\n")
		os.Exit(1)
	}

	// parsing parameters
	config := ParseParameters(c)

	if config.ImportBuildDashboard {
		dfltConfig := InitConfig()
		dfltConfig.LoadCfgFile()

		config.GrafanaAccess = dfltConfig.GrafanaAccess
		config.GrafanaUrl = dfltConfig.GrafanaUrl
		config.GrafanaDatasource = dfltConfig.GrafanaDatasource
		config.GrafanaUser = dfltConfig.GrafanaUser
		config.GrafanaPassword = dfltConfig.GrafanaPassword
	}

	influxdb := influxdbclient.NewInfluxDB(config.InfluxdbServer, config.InfluxdbPort, config.InfluxdbDatabase, config.InfluxdbUser, config.InfluxdbPassword)
	influxdb.SetDebug(config.Debug)
	err := influxdb.Connect()
	check(err)

	if exist, _ := influxdb.ExistDB(config.InfluxdbDatabase); exist != true {
		_, err := influxdb.CreateDB(config.InfluxdbDatabase)
		check(err)
	}

	influxdbLog := influxdbclient.NewInfluxDB(config.InfluxdbServer, config.InfluxdbPort, config.ImportLogDatabase, config.InfluxdbUser, config.InfluxdbPassword)
	influxdbLog.SetDebug(config.Debug)
	err = influxdbLog.Connect()
	check(err)

	if exist, _ := influxdbLog.ExistDB(config.ImportLogDatabase); exist != true {
		_, err := influxdbLog.CreateDB(config.ImportLogDatabase)
		check(err)
		influxdbLog.SetRetentionPolicy("log_retention", config.ImportLogRetention, true)

	} else {
		influxdbLog.UpdateRetentionPolicy("log_retention", config.ImportLogRetention, true)
	}

	nmonFiles := new(NmonFiles)

	for _, param := range c.Args() {
		paraminfo, err := os.Stat(param)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Printf("%s doesn't exist ! skipped.\n", param)
			}
			continue
		}

		if paraminfo.IsDir() {
			entries, err := ioutil.ReadDir(param)
			check(err)
			for _, entry := range entries {
				if !entry.IsDir() {
					file := path.Join(param, entry.Name())
					nmonFiles.Add(file, path.Ext(file))
				}
			}
			continue
		}
		nmonFiles.Add(param, path.Ext(param))
	}

	for _, nmon_file := range nmonFiles.Valid() {
		var count int64
		count = 0
		nmon := InitNmon(config, nmon_file.Name)
		file, err := os.Open(nmon_file.Name)
		check(err)

		defer file.Close()
		reader := bufio.NewReader(file)
		scanner := bufio.NewScanner(reader)
		scanner.Split(bufio.ScanLines)

		var lines []string

		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}

		sort.Strings(lines)

		var userSkipRegexp *regexp.Regexp

		if len(config.ImportSkipMetrics) > 0 {
			skipped := strings.Replace(config.ImportSkipMetrics, ",", "|", -1)
			userSkipRegexp = regexp.MustCompile(skipped)
		}

		var last string
		filters := new(influxdbclient.Filters)
		filters.Add("file", path.Base(nmon_file.Name), "text")

		result, err := influxdbLog.ReadFirstPoint("value", filters, "timestamp")
		check(err)

		var lastTime time.Time
		if len(result) > 0 {
			lastTime, err = ConvertTimeStamp(result[1].(string), nmon.Config.Timezone)
		} else {
			lastTime, err = ConvertTimeStamp("00:00:00,01-JAN-1900", nmon.Config.Timezone)
		}
		check(err)

		orig_checksum, err := influxdbLog.ReadFirstPoint("value", filters, "checksum")
		check(err)

		checksum, err := Checksum(nmon_file.Name)
		check(err)
		ckfield := map[string]interface{}{"value": checksum}
		if len(orig_checksum) > 0 {

			if orig_checksum[1].(string) == checksum {
				fmt.Printf("file not changed since last import: %s\n", nmon_file.Name)
				continue
			}
		}
		for _, line := range lines {

			if cpuallRegexp.MatchString(line) && !config.ImportAllCpus {
				continue
			}

			if diskallRegexp.MatchString(line) && config.ImportSkipDisks {
				continue
			}

			if skipRegexp.MatchString(line) {
				continue
			}

			if statsRegexp.MatchString(line) {
				matched := statsRegexp.FindStringSubmatch(line)
				elems := strings.Split(line, ",")
				name := elems[0]

				if len(config.ImportSkipMetrics) > 0 {
					if userSkipRegexp.MatchString(name) {
						if nmon.Debug {
							fmt.Printf("metric skipped : %s\n", name)
						}
						continue
					}
				}

				timeStr, err := nmon.GetTimeStamp(matched[1])
				check(err)
				last = timeStr
				timestamp, err := ConvertTimeStamp(timeStr, nmon.Config.Timezone)
				if timestamp.Before(lastTime) && !nmon.Config.ImportForce {
					continue
				}

				for i, value := range elems[2:] {
					if len(nmon.DataSeries[name].Columns) < i+1 {
						if nmon.Debug {
							fmt.Printf(line)
							fmt.Printf("Entry added position %d in serie %s since nmon start: skipped\n", i+1, name)
						}
						continue
					}
					column := nmon.DataSeries[name].Columns[i]
					tags := map[string]string{"host": nmon.Hostname, "name": column}

					// try to convert string to integer
					converted, err := strconv.ParseFloat(value, 64)
					if err != nil {
						//if not working, skip to next value. We don't want text values in InfluxDB.
						continue
					}

					//send integer if it worked
					field := map[string]interface{}{"value": converted}

					measurement := ""
					if nfsRegexp.MatchString(name) {
						measurement = name
					} else {
						measurement = nameRegexp.ReplaceAllString(name, "")
					}

					influxdb.AddPoint(measurement, timestamp, field, tags)

					if influxdb.PointsCount() == 10000 {
						err = influxdb.WritePoints()
						check(err)
						count += influxdb.PointsCount()
						influxdb.ClearPoints()
						fmt.Printf("#")
					}
				}
			}

			if topRegexp.MatchString(line) {
				matched := topRegexp.FindStringSubmatch(line)
				elems := strings.Split(line, ",")
				name := elems[0]
				if len(config.ImportSkipMetrics) > 0 {
					if userSkipRegexp.MatchString(name) {
						if nmon.Debug {
							fmt.Printf("metric skipped : %s\n", name)
						}
						continue
					}
				}

				timeStr, err := nmon.GetTimeStamp(matched[1])
				check(err)
				timestamp, err := ConvertTimeStamp(timeStr, nmon.Config.Timezone)

				if len(elems) < 14 {
					fmt.Printf("error TOP import:")
					fmt.Println(elems)
					continue
				}

				for i, value := range elems[3:12] {
					column := nmon.DataSeries["TOP"].Columns[i]

					var wlmclass string
					if len(elems) < 15 {
						wlmclass = "none"
					} else {
						wlmclass = elems[14]
					}

					tags := map[string]string{"host": nmon.Hostname, "name": column, "pid": elems[1], "command": elems[13], "wlm": wlmclass}

					// try to convert string to integer
					converted, err := strconv.ParseFloat(value, 64)
					if err != nil {
						//if not working, skip to next value. We don't want text values in InfluxDB.
						continue
					}

					//send integer if it worked
					field := map[string]interface{}{"value": converted}

					influxdb.AddPoint("TOP", timestamp, field, tags)

					if influxdb.PointsCount() == 10000 {
						err = influxdb.WritePoints()
						check(err)
						count += influxdb.PointsCount()
						influxdb.ClearPoints()
						fmt.Printf("#")
					}
				}
			}
		}
		// flushing remaining data
		influxdb.WritePoints()
		count += influxdb.PointsCount()
		fmt.Printf("\nFile %s imported : %d points !\n", nmon_file.Name, count)
		if config.ImportBuildDashboard {
			NmonDashboardFile(config, nmon_file.Name)
		}

		if len(last) > 0 {
			field := map[string]interface{}{"value": last}
			tag := map[string]string{"file": path.Base(nmon_file.Name)}
			lasttime, _ := ConvertTimeStamp("00:00:00,01-JAN-2000", nmon.Config.Timezone)
			influxdbLog.AddPoint("timestamp", lasttime, field, tag)

			influxdbLog.AddPoint("checksum", lasttime, ckfield, tag)
			err = influxdbLog.WritePoints()
			check(err)
		}
	}
}
