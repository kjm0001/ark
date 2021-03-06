/*
Copyright 2017 the Heptio Ark contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"time"

	pkgbackup "github.com/heptio/ark/pkg/backup"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/tools/cache"

	arkv1client "github.com/heptio/ark/pkg/generated/clientset/versioned/typed/ark/v1"
	informers "github.com/heptio/ark/pkg/generated/informers/externalversions/ark/v1"
	listers "github.com/heptio/ark/pkg/generated/listers/ark/v1"
)

// gcController creates DeleteBackupRequests for expired backups.
type gcController struct {
	*genericController

	logger                    logrus.FieldLogger
	backupLister              listers.BackupLister
	deleteBackupRequestClient arkv1client.DeleteBackupRequestsGetter
	syncPeriod                time.Duration

	clock clock.Clock
}

// NewGCController constructs a new gcController.
func NewGCController(
	logger logrus.FieldLogger,
	backupInformer informers.BackupInformer,
	deleteBackupRequestClient arkv1client.DeleteBackupRequestsGetter,
	syncPeriod time.Duration,
) Interface {
	if syncPeriod < time.Minute {
		logger.WithField("syncPeriod", syncPeriod).Info("Provided GC sync period is too short. Setting to 1 minute")
		syncPeriod = time.Minute
	}

	c := &gcController{
		genericController:         newGenericController("gc-controller", logger),
		syncPeriod:                syncPeriod,
		clock:                     clock.RealClock{},
		backupLister:              backupInformer.Lister(),
		deleteBackupRequestClient: deleteBackupRequestClient,
		logger: logger,
	}

	c.syncHandler = c.processQueueItem
	c.cacheSyncWaiters = append(c.cacheSyncWaiters, backupInformer.Informer().HasSynced)

	c.resyncPeriod = syncPeriod
	c.resyncFunc = c.enqueueAllBackups

	backupInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    c.enqueue,
			UpdateFunc: func(_, obj interface{}) { c.enqueue(obj) },
		},
	)

	return c
}

// enqueueAllBackups lists all backups from cache and enqueues all of them so we can check each one
// for expiration.
func (c *gcController) enqueueAllBackups() {
	c.logger.Debug("gcController.enqueueAllBackups")

	backups, err := c.backupLister.List(labels.Everything())
	if err != nil {
		c.logger.WithError(errors.WithStack(err)).Error("error listing backups")
		return
	}

	for _, backup := range backups {
		c.enqueue(backup)
	}
}

func (c *gcController) processQueueItem(key string) error {
	log := c.logger.WithField("backup", key)

	ns, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return errors.Wrap(err, "error splitting queue key")
	}

	backup, err := c.backupLister.Backups(ns).Get(name)
	if apierrors.IsNotFound(err) {
		log.Debug("Unable to find backup")
		return nil
	}
	if err != nil {
		return errors.Wrap(err, "error getting backup")
	}

	log = c.logger.WithFields(
		logrus.Fields{
			"backup":     key,
			"expiration": backup.Status.Expiration.Time,
		},
	)

	now := c.clock.Now()

	expiration := backup.Status.Expiration.Time
	if expiration.IsZero() || expiration.After(now) {
		log.Debug("Backup has not expired yet, skipping")
		return nil
	}

	log.Info("Backup has expired. Creating a DeleteBackupRequest.")

	req := pkgbackup.NewDeleteBackupRequest(backup.Name, string(backup.UID))

	_, err = c.deleteBackupRequestClient.DeleteBackupRequests(ns).Create(req)
	if err != nil {
		return errors.Wrap(err, "error creating DeleteBackupRequest")
	}

	return nil
}
