// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/event"
	tsuruIo "github.com/tsuru/tsuru/io"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/provision"
	"gopkg.in/mgo.v2/bson"
)

type DeployKind string

const (
	DeployArchiveURL  DeployKind = "archive-url"
	DeployGit         DeployKind = "git"
	DeployImage       DeployKind = "image"
	DeployRollback    DeployKind = "rollback"
	DeployUpload      DeployKind = "upload"
	DeployUploadBuild DeployKind = "uploadbuild"
)

type DeployData struct {
	ID          bson.ObjectId `bson:"_id,omitempty"`
	App         string
	Timestamp   time.Time
	Duration    time.Duration
	Commit      string
	Error       string
	Image       string
	Log         string
	User        string
	Origin      string
	CanRollback bool
	RemoveDate  time.Time `bson:",omitempty"`
	Diff        string
}

// ListDeploys returns the list of deploy that match a given filter.
func ListDeploys(filter *Filter, skip, limit int) ([]DeployData, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	appsList, err := List(filter)
	if err != nil {
		return nil, err
	}
	apps := make([]string, len(appsList))
	for i, a := range appsList {
		apps[i] = a.GetName()
	}
	var list []DeployData
	f := bson.M{"app": bson.M{"$in": apps}, "removedate": bson.M{"$exists": false}}
	s := bson.M{
		"app":         1,
		"timestamp":   1,
		"duration":    1,
		"commit":      1,
		"error":       1,
		"image":       1,
		"user":        1,
		"origin":      1,
		"canrollback": 1,
		"removedate":  1,
	}
	query := conn.Deploys().Find(f).Select(s).Sort("-timestamp")
	if skip != 0 {
		query = query.Skip(skip)
	}
	if limit != 0 {
		query = query.Limit(limit)
	}
	if err = query.All(&list); err != nil {
		return nil, err
	}
	validImages := set{}
	for _, appName := range apps {
		var imgs []string
		imgs, err = Provisioner.ValidAppImages(appName)
		if err != nil {
			return nil, err
		}
		validImages.Add(imgs...)
	}
	for i := range list {
		list[i].CanRollback = validImages.Includes(list[i].Image)
		r := regexp.MustCompile("v[0-9]+$")
		if list[i].Image != "" && r.MatchString(list[i].Image) {
			parts := r.FindAllStringSubmatch(list[i].Image, -1)
			list[i].Image = parts[0][0]
		}
	}
	return list, err
}

func GetDeploy(id string) (*DeployData, error) {
	var dep DeployData
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if !bson.IsObjectIdHex(id) {
		return nil, fmt.Errorf("id parameter is not ObjectId: %s", id)
	}
	if err := conn.Deploys().FindId(bson.ObjectIdHex(id)).One(&dep); err != nil {
		return nil, err
	}
	return &dep, nil
}

type DeployOptions struct {
	App          *App
	Commit       string
	ArchiveURL   string
	FileSize     int64
	File         io.ReadCloser `bson:"-"`
	OutputStream io.Writer     `bson:"-"`
	User         string
	Image        string
	Origin       string
	Rollback     bool
	Build        bool
	Event        *event.Event `bson:"-"`
	Kind         DeployKind
}

func (o *DeployOptions) GetKind() (kind DeployKind) {
	defer func() {
		o.Kind = kind
	}()
	if o.Rollback {
		return DeployRollback
	}
	if o.Image != "" {
		return DeployImage
	}
	if o.File != nil {
		if o.Build {
			return DeployUploadBuild
		}
		return DeployUpload
	}
	if o.Commit != "" {
		return DeployGit
	}
	return DeployArchiveURL
}

// Deploy runs a deployment of an application. It will first try to run an
// archive based deploy (if opts.ArchiveURL is not empty), and then fallback to
// the Git based deployment.
func Deploy(opts DeployOptions) (string, error) {
	if opts.Event == nil {
		return "", fmt.Errorf("missing event in deploy opts")
	}
	if opts.Rollback && !regexp.MustCompile(":v[0-9]+$").MatchString(opts.Image) {
		img, err := GetImage(opts.App.Name, opts.Image)
		if err == nil {
			opts.Image = img
		}
	}
	logWriter := LogWriter{App: opts.App}
	logWriter.Async()
	defer logWriter.Close()
	eventWriter := opts.Event.GetLogWriter()
	writer := io.MultiWriter(&tsuruIo.NoErrorWriter{Writer: opts.OutputStream}, eventWriter, &logWriter)
	imageId, err := deployToProvisioner(&opts, writer)
	if err != nil {
		return "", err
	}
	err = incrementDeploy(opts.App)
	if err != nil {
		log.Errorf("WARNING: couldn't increment deploy count, deploy opts: %#v", opts)
	}
	if opts.App.UpdatePlatform == true {
		opts.App.SetUpdatePlatform(false)
	}
	return imageId, nil
}

func deployToProvisioner(opts *DeployOptions, writer io.Writer) (string, error) {
	switch opts.GetKind() {
	case DeployRollback:
		return Provisioner.Rollback(opts.App, opts.Image, writer)
	case DeployImage:
		if deployer, ok := Provisioner.(provision.ImageDeployer); ok {
			return deployer.ImageDeploy(opts.App, opts.Image, writer)
		}
		fallthrough
	case DeployUpload, DeployUploadBuild:
		if deployer, ok := Provisioner.(provision.UploadDeployer); ok {
			return deployer.UploadDeploy(opts.App, opts.File, opts.FileSize, opts.Build, writer)
		}
		fallthrough
	default:
		return Provisioner.(provision.ArchiveDeployer).ArchiveDeploy(opts.App, opts.ArchiveURL, writer)
	}
}

func ValidateOrigin(origin string) bool {
	originList := []string{"app-deploy", "git", "rollback", "drag-and-drop", "image"}
	for _, ol := range originList {
		if ol == origin {
			return true
		}
	}
	return false
}

func incrementDeploy(app *App) error {
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Apps().Update(
		bson.M{"name": app.Name},
		bson.M{"$inc": bson.M{"deploys": 1}},
	)
	if err == nil {
		app.Deploys += 1
	}
	return err
}

func GetImage(appName, img string) (string, error) {
	conn, err := db.Conn()
	if err != nil {
		return "", err
	}
	defer conn.Close()
	var deploy DeployData
	qApp := bson.M{"app": appName}
	qImage := bson.M{"$or": []bson.M{{"image": img}, {"image": bson.M{"$regex": ".*:" + img + "$"}}}}
	query := bson.M{"$and": []bson.M{qApp, qImage}}
	if err := conn.Deploys().Find(query).One(&deploy); err != nil {
		return "", err
	}
	return deploy.Image, nil
}
