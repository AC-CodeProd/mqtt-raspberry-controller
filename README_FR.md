# mqtt-raspberry-controller

[English](README.md) | **Français**

Démon générique de contrôle de GPIO et de commandes via MQTT, écrit en Go.

Sous [licence MIT](LICENSE). Documentation d'exploitation : [configuration](docs/CONFIGURATION.md), [installation et mise à niveau](docs/INSTALL.md), [sécurité](docs/SECURITY.md), [dépannage](docs/TROUBLESHOOTING.md) et [processus de publication](docs/RELEASE.md).

## Entités prises en charge

- `switch`
  - état provenant d'une sortie GPIO ou d'un état en mémoire
  - séquences d'actions ON/OFF configurables
- `button`
  - séquence d'actions PRESS configurable
- `binary_sensor`
  - entrée GPIO avec détection de fronts

## Actions prises en charge

- `gpio` : définir une sortie GPIO nommée
- `command` : exécuter directement un chemin absolu vers un exécutable, sans shell ni recherche dans `PATH`
- `delay` : attendre pendant une durée donnée
- `mqtt` : publier un autre message MQTT

Les actions sont exécutées séquentiellement. Définissez `ignore_error: true` sur une action lorsqu'un échec ne doit pas interrompre le reste de la séquence.

### Isolation des commandes externes

Une action `command` est validée avant le démarrage. `command` doit être un chemin absolu vers un fichier ordinaire existant dont au moins un bit de mode d'exécution est activé ; il n'est jamais résolu via `PATH`. Un `working_dir` facultatif doit être un répertoire absolu existant. Les noms de variables d'environnement doivent correspondre à `[A-Za-z_][A-Za-z0-9_]*`, et les champs de commande, arguments, répertoires de travail et valeurs d'environnement ne doivent pas contenir d'octets NUL. Les arguments sont transmis directement à l'exécutable ; la syntaxe du shell, les expansions, les pipelines et les redirections ne sont pas interprétés.

Les commandes reçoivent un environnement de base déterministe contenant uniquement `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`, `LANG=C` et `LC_ALL=C`, fusionné avec la table `env` explicite de l'action. Le démon ne copie jamais son propre environnement dans une commande ; les variables liées aux identifiants MQTT et systemd sont donc absentes, sauf si l'action les configure explicitement. Une action peut remplacer explicitement n'importe laquelle des trois valeurs de base.

La sortie standard et la sortie d'erreur standard partagent une capture de 64 Kio sûre en concurrence. Toute sortie supplémentaire est abandonnée, tandis que le total des octets, avec saturation, et l'état de troncature continuent d'être suivis. Le contenu des sorties, les arguments et les valeurs d'environnement ne sont jamais renvoyés dans les erreurs ni écrits dans les journaux ; seules les métadonnées de nombre d'octets et de troncature sont signalées.

Sous Linux, chaque commande démarre dans un nouveau groupe de processus. En cas de dépassement du délai ou d'annulation lors de l'arrêt, le superviseur envoie `SIGTERM` à ce groupe tant que le PID du processus meneur lui appartient encore, accorde un délai de grâce borné, puis envoie `SIGKILL` ; une attente bornée des tubes empêche également les descendants qui conservent stdout ou stderr de bloquer l'exécution. En cas de terminaison normale, aucun signal n'est envoyé au groupe de processus après la récupération du processus meneur, ce qui évite tout risque de signaler un PID réutilisé sans rapport. Les commandes ne doivent donc pas lancer de services en arrière-plan : un processus enfant survivant n'est pas pris en charge et est nettoyé lorsque systemd arrête l'unité via `KillMode=control-group`. Un programme qui se démonise volontairement avec `setsid(2)` échappe au groupe de processus propre à l'action, mais reste soumis au cgroup du service lors de l'arrêt de l'unité ; ces programmes ne sont pas pris en charge, car le respect de leur délai d'expiration individuel ne peut pas être garanti.

## Sécurité des commandes MQTT et arrêt

- Les messages de contrôle sur les topics `set` et `press` ne doivent pas être conservés. Les commandes conservées sont rejetées afin qu'une reconnexion ne puisse pas rejouer une action.
- Au sein d'une même connexion MQTT, les retransmissions QoS 1/2 déjà acceptées par ce processus sont dédupliquées selon le topic et l'identifiant de paquet au moyen d'un cache borné à 256 entrées. Le cache est réinitialisé lors d'une reconnexion, car les identifiants de paquet sont propres à la session ; un paquet DUP qui n'a pas déjà été observé par ce processus est accepté afin d'éviter la perte d'une commande. Les publications d'état et de Discovery restent conservées.
- La charge utile des commandes est limitée à 4096 octets. Chaque entité dispose d'une file FIFO de 32 commandes ; une commande reçue lorsque cette file est pleine est rejetée et journalisée avec limitation de débit. Les entités distinctes disposent de workers indépendants. L'ordre FIFO commence selon l'ordre dans lequel Paho transmet les callbacks ; l'ordre MQTT entre niveaux de QoS différents dépend toujours des paramètres du broker et des messages en vol.
- À la réception de SIGINT/SIGTERM, le démon cesse d'accepter des commandes et utilise un budget d'arrêt unique de 10 secondes pour les workers du contrôleur et les callbacks MQTT. Il annule les délais et commandes externes actifs, abandonne les commandes en attente, republie si possible le dernier état observable d'un switch annulé, publie `offline`, se déconnecte de MQTT, puis ferme enfin le GPIO. Si le délai expire, `run` se termine sans fermer simultanément les ressources et systemd met fin au processus en échec.

## Cycle de vie de Home Assistant Discovery

Les topics Discovery sont conservés et réconciliés à chaque connexion MQTT. Le démon enregistre les topics exacts qu'il a pu publier dans `mqtt.discovery.state_file` (par défaut `/var/lib/mqtt-raspberry-controller/discovery-state.json`) et lie ce manifeste au `mqtt.client_id` stable. Avant de publier la configuration actuelle, il envoie une charge utile vide conservée, avec un niveau de QoS acquitté (au moins QoS 1), aux topics suivis devenus obsolètes. Cela couvre la suppression d'une entité, le renommage de son identifiant, le changement de type de composant, de préfixe Discovery ou d'identifiant d'appareil, ainsi que `discovery.enabled: false`. La mise à jour de l'état est écrite atomiquement avant les modifications sur le broker, sous forme de surapproximation ; ainsi, une réconciliation interrompue ou partiellement échouée est conservée pour le prochain appel explicite, redémarrage ou reconnexion MQTT, au lieu d'être oubliée.

Le fichier d'état est traité comme un registre de propriété et non comme une liste de nettoyage arbitraire. Il doit s'agir d'un fichier ordinaire `0600` appartenant à l'utilisateur du service, situé dans un répertoire appartenant au service, non inscriptible par le groupe ou les autres utilisateurs et ne contenant aucun composant de chemin symbolique. Un verrou non bloquant empêche deux processus locaux de réconcilier simultanément le même manifeste. Chaque entrée doit présenter la structure Discovery prise en charge, l'identifiant d'appareil enregistré et le même `mqtt.client_id` ; un manifeste mal formé, étranger ou partagé interrompt la réconciliation sans rien publier ni supprimer. L'unité systemd fournie crée le répertoire d'état privé inscriptible avec le mode `0700`. Conservez un fichier d'état et un identifiant client MQTT stable et unique par contrôleur. Lors du changement de `device.id`, conservez `mqtt.client_id` afin que la propriété précédente puisse être authentifiée. Un chemin personnalisé situé hors de `/var/lib/mqtt-raspberry-controller` nécessite également une surcharge systemd correspondante avec `StateDirectory=` ou un `ReadWritePaths=` strictement limité.

### Limites du nettoyage manuel

Le contrôleur ne peut nettoyer que les topics Discovery enregistrés dans son fichier d'état de propriété actuel. Si ce fichier est perdu, ou si un topic a été créé hors de ce contrôleur, supprimez explicitement chaque topic obsolète connu avec les mêmes identifiants d'accès au broker et une publication vide conservée :

```bash
install -d -m 0700 "$HOME/.config"
install -m 0600 /dev/null "$HOME/.config/mosquitto_pub"
# Edit that private Mosquitto client config with one option per line, including:
# -h mqtt.example.net
# -p 8883
# --cafile /path/to/ca.pem
# -u mqtt-raspberry-controller
# -P THE_PASSWORD
mosquitto_pub \
  --topic homeassistant/switch/OLD_DEVICE_ID/OLD_ENTITY_ID/config \
  --null-message --retain --qos 1
```

N'utilisez jamais de joker pour le nettoyage : les topics MQTT `PUBLISH` ne peuvent pas contenir de jokers, et une suppression large risquerait d'affecter d'autres appareils. Lorsqu'une ACL énumère des topics exacts, conservez l'accès en écriture aux anciens topics Discovery jusqu'au premier redémarrage réussi suivant un renommage ou une suppression ; ne retirez ces anciennes lignes de l'ACL qu'après le nettoyage. Le changement de `device.id` exige de même un accès en écriture temporaire aux anciens comme aux nouveaux topics exacts. Si le fichier d'état est perdu après la création de topics obsolètes, le démon ne peut délibérément pas retrouver leur propriété à partir du broker ; procédez à un nettoyage manuel explicite.

Aucun abonnement au topic de naissance de Home Assistant n'est nécessaire. Home Assistant reçoit ces configurations Discovery conservées lorsqu'il s'abonne après son démarrage, tandis que le contrôleur republie déjà lors de sa propre reconnexion MQTT. Une republication déclenchée par la naissance du service dupliquerait le trafic du broker sans améliorer la reprise pour les configurations conservées.

## Compilation

La compatibilité du code source commence avec Go 1.24, car `github.com/eclipse/paho.mqtt.golang v1.5.1` l'exige. La CI et les versions publiées utilisent exactement la chaîne d'outils de sécurité prise en charge définie dans `.go-version` (actuellement Go 1.26.8).

```bash
go mod download
make build VERSION=1.0.0
./bin/mqtt-raspberry-controller --version
```

Utilisez `make check` pour exécuter la compilation, la vérification du formatage, les tests, vet, la vérification de propreté des modules et les contrôles de paquetage. `make ci` exécute également la suite avec détection des accès concurrents. `make coverage` écrit `bin/coverage.out`, et `make clean` supprime les artefacts de compilation et de publication générés. Consultez [docs/RELEASE.md](docs/RELEASE.md) pour les archives reproductibles, les sommes de contrôle, les SBOM et les critères de publication.

## Tests

La suite par défaut est indépendante du matériel et ne nécessite aucun broker :

```bash
go test ./...
go test -race ./...
make check
```

Le comportement GPIO est exercé par l'intermédiaire de la petite interface de demande de ligne du gestionnaire, notamment l'ouverture des sorties et entrées, l'état logique, la gestion des fronts, le retour arrière, les erreurs et le comportement de fermeture. Cette couverture portable avec de fausses lignes s'exécute dans la CI Linux et sur les machines des développeurs sans accorder d'accès au matériel GPIO. Les tests avec `gpio-sim`/configfs et sur un Raspberry Pi physique restent facultatifs, car les modules du noyau, les permissions configfs et la disponibilité des lignes varient selon l'exécuteur ; utilisez le test rapide Raspberry Pi fourni ci-dessous pour valider ensemble une carte et son noyau.

Un véritable test d'intégration MQTT est facultatif et doit être activé explicitement. Faites pointer `MQTT_TEST_BROKER` vers un broker de test anonyme éphémère ; le test crée des identifiants client et des topics uniques de manière cryptographiquement sûre, vérifie la livraison des commandes en QoS 1 et contrôle la disponibilité `online`/`offline` conservée :

```bash
docker run --detach --rm --name mqtt-raspberry-controller-test \
  --publish 127.0.0.1:1883:1883 \
  --volume "$PWD/packaging/mosquitto/test.conf:/mosquitto/config/mosquitto.conf:ro" \
  eclipse-mosquitto:2.0.20
trap 'docker stop mqtt-raspberry-controller-test >/dev/null' EXIT
MQTT_TEST_BROKER=tcp://127.0.0.1:1883 go test -race ./mqtt
```

GitHub Actions utilise la même version de Mosquitto épinglée par digest et la même configuration, attend que le broker soit prêt, puis exécute le formatage, la vérification de propreté des modules, vet, les tests ordinaires, les tests avec détection des accès concurrents adossés au broker, staticcheck, govulncheck, une assertion de compilation statique versionnée et les contrôles de paquetage. Les GPIO physiques restent **NON EXÉCUTÉS** dans la CI hébergée.

## Installation rapide

Installez la dernière version Linux publiée avec :

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/install.sh | sh
```

Le programme d'installation détecte `amd64` ou `arm64`, télécharge l'archive publiée correspondante, la vérifie avec le fichier `SHA256SUMS` de la release, contrôle la version intégrée sans exécuter le binaire téléchargé en tant que root, puis installe le binaire, l'unité systemd, la règle udev, la déclaration sysusers, l'exemple de configuration et le fichier de mot de passe à accès restreint. Il n'utilise `sudo` qu'après avoir basculé vers le programme d'installation stocké dans le tag de release sélectionné, effectue les opérations privilégiées dans un répertoire temporaire appartenant à root, conserve la configuration et le fichier de mot de passe existants, restaure les fichiers remplacés si l'installation échoue et ne démarre volontairement pas le service avant sa configuration.

Lors d'une nouvelle installation disposant d'un terminal de contrôle, le programme propose un assistant MQTT interactif. Il demande l'URL du broker, le nom d'utilisateur, un identifiant d'instance et un mot de passe masqué saisi deux fois. La configuration produite est validée sous le compte de service non privilégié avant de remplacer les fichiers d'exemple. Utilisez `--no-configure` pour ignorer l'assistant ou `--configure` pour l'imposer. Sur une installation existante, l'assistant forcé modifie uniquement les champs canoniques du broker, du nom d'utilisateur et du mot de passe ; il refuse une structure YAML non prise en charge au lieu d'appliquer une mise à jour partielle :

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/install.sh | sh -s -- --configure
```

L'assistant configure MQTT ainsi qu'une identité unique de client et d'appareil. Les identifiants d'instance sont normalisés en minuscules et doivent commencer par une lettre ou un chiffre afin que le champ `device.id` produit soit valide. L'assistant ne démarre pas le service et ne décide pas quelles lignes GPIO peuvent être utilisées sans risque sur le matériel connecté. Pour un broker `mqtt://`, `tcp://` ou `ws://`, il affiche un avertissement sur le transport en clair et exige une confirmation explicite avant d'activer `allow_insecure_transport: true` et de supprimer les options TLS incompatibles ; privilégiez TLS chaque fois que possible.

`SHA256SUMS` détecte une corruption, mais n'authentifie pas l'éditeur. Pour les releases publiques étiquetées, le workflow crée également une attestation GitHub de provenance du build. Lorsque GitHub CLI est installé, le programme d'installation vérifie automatiquement cette attestation ; définissez `MQTT_RASPBERRY_REQUIRE_ATTESTATION=1` avant `sh` pour refuser l'installation lorsque la provenance ne peut pas être vérifiée. La commande pratique fondée sur `main` fait nécessairement confiance à l'état actuel de la branche par défaut. Pour une installation reproductible, remplacez `X.Y.Z` et récupérez le programme d'installation depuis le tag immuable de cette release :

```bash
version=X.Y.Z
curl -fsSL "https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/tags/v${version}/scripts/install.sh" | MQTT_RASPBERRY_VERSION="$version" MQTT_RASPBERRY_REQUIRE_ATTESTATION=1 sh
```

### Désinstallation

Supprimez le programme et son intégration au service tout en conservant la configuration, les identifiants, l'état Discovery et le compte du service :

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/uninstall.sh | sh -s -- --remove
```

Pour supprimer définitivement la configuration par défaut, le fichier de mot de passe, l'état Discovery, l'utilisateur du service et son groupe, utilisez le mode destructif explicite :

```bash
curl -fsSL https://raw.githubusercontent.com/AC-CodeProd/mqtt-raspberry-controller/refs/heads/main/scripts/uninstall.sh | sh -s -- --clear
```

`--clear` conserve le groupe partagé `gpio` et ne peut pas découvrir les chemins personnalisés de configuration, d'identifiants ou d'état situés hors des répertoires par défaut.

## Installation

Le service fourni utilise un compte `mqtt-raspberry-controller` statique et non privilégié. Créez le groupe GPIO si la distribution ne le fournit pas, puis installez la déclaration sysusers (ou utilisez la solution de repli `useradd` indiquée) :

```bash
make build VERSION=1.0.0
getent group gpio >/dev/null || sudo groupadd --system gpio
sudo install -m 0644 packaging/sysusers.d/mqtt-raspberry-controller.conf /usr/lib/sysusers.d/mqtt-raspberry-controller.conf
sudo systemd-sysusers mqtt-raspberry-controller.conf

# Fallback on systems without systemd-sysusers (do not run if the account exists):
# sudo groupadd --system mqtt-raspberry-controller
# sudo useradd --system --gid mqtt-raspberry-controller --home-dir /nonexistent \
#   --shell /usr/sbin/nologin --comment 'MQTT Raspberry Controller' mqtt-raspberry-controller

sudo install -m 0755 bin/mqtt-raspberry-controller /usr/local/bin/mqtt-raspberry-controller
sudo install -d -o root -g mqtt-raspberry-controller -m 0750 /etc/mqtt-raspberry-controller
sudo install -o root -g mqtt-raspberry-controller -m 0640 configs/config.example.yaml /etc/mqtt-raspberry-controller/config.yaml
sudo install -o root -g root -m 0600 /dev/null /etc/mqtt-raspberry-controller/mqtt-password
systemd-ask-password 'Password for the mqtt-raspberry-controller MQTT user' | sudo tee /etc/mqtt-raspberry-controller/mqtt-password >/dev/null
sudo install -m 0644 packaging/udev/60-mqtt-raspberry-controller.rules /etc/udev/rules.d/60-mqtt-raspberry-controller.rules
sudo install -m 0644 packaging/systemd/mqtt-raspberry-controller.service /etc/systemd/system/mqtt-raspberry-controller.service
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=gpio --sysname-match=gpiochip0
```

La configuration doit appartenir à `root:mqtt-raspberry-controller` afin que le compte du service puisse l'ouvrir ; conservez son mode à `0640` ou plus restrictif. La source du mot de passe doit être un fichier ordinaire dont le mode exact est `0400`, `0600` ou `0640` ; la copie privée des identifiants par systemd est en `0400`. L'unité l'importe avec `LoadCredential=`, et le démon lit uniquement cette copie privée via la variable de chemin `MQTT_PASSWORD_FILE` ; ne placez pas de mots de passe dans les URI du broker, les environnements de processus ou un YAML lisible par tous. Hors de systemd, exportez directement `MQTT_PASSWORD_FILE` avec le chemin d'un fichier soumis aux mêmes restrictions. Le champ `password` en ligne reste pris en charge pour des raisons de compatibilité, mais son utilisation est déconseillée. L'unité fournie utilise délibérément des affectations `Environment=` explicites pour les chemins des identifiants et ne charge pas de fichier `EnvironmentFile=` au format shell.

### Expansion des variables de configuration

Le démon décode strictement le YAML, y compris en rejetant les champs inconnus, avant de développer les références d'environnement dans les valeurs de chaîne modélisées. Utilisez `${NAME}`, où les noms correspondent à `[A-Za-z_][A-Za-z0-9_]*`. L'expansion s'effectue en une seule passe : le texte fourni par une variable est inséré tel quel et n'est jamais développé de nouveau. Utilisez `$$` pour un signe dollar littéral, de sorte que `$${NAME}` produise le texte littéral `${NAME}`. Un `$` isolé, `$NAME`, `${}` et les expressions mal formées telles que `${BAD-NAME}` restent littéraux. Une référence `${NAME}` valide dont la variable n'est pas définie constitue une erreur qui indique le chemin du champ et le nom de la variable sans afficher le champ ni la valeur substituée.

L'expansion n'est autorisée que dans les valeurs de chaîne suivantes :

- MQTT : `broker`, `username`, `password`, `password_file`, `client_id`, `base_topic`, `keep_alive`, `connect_timeout` ; toutes les chaînes sous `tls` ; ainsi que `discovery.prefix` et `discovery.state_file`.
- Appareil : `id`, `name`, `manufacturer` et `model`.
- GPIO : le `chip` de chaque sortie, ainsi que le `chip`, le `bias` et le `debounce` de chaque entrée.
- Entités : `id`, `type`, `name`, `icon`, `device_class` ; `state.type`, `state.gpio`, `source.type` et `source.gpio`.
- Actions dans `on`, `off` et `press` : `type`, `gpio`, `command`, chaque entrée de `args`, `working_dir`, chaque valeur de `env`, `timeout`, `duration`, `topic` et `payload`.

Les clés de map (notamment les noms de GPIO et les noms d'environnement des actions), la structure YAML, les nombres et les booléens ne sont jamais développés. Puisque la substitution intervient après le décodage, les valeurs contenant de la ponctuation YAML, des guillemets, `#` ou des retours à la ligne restent une seule chaîne et ne peuvent pas créer de clés ni d'éléments de liste. Placez les scalaires sources entre guillemets lorsque YAML l'exige ; l'expansion ne réinterprète pas la valeur insérée comme du YAML.

La règle udev ne correspond délibérément qu'à `gpiochip0` et impose le propriétaire `root`, le groupe `gpio` et le mode `0660`. Les permissions des périphériques de caractères GPIO sous Linux s'appliquent **au niveau de la puce, et non de la ligne** : udev ne peut pas accorder l'accès à certains offsets d'une puce. La validation de la configuration et la discipline de câblage doivent donc limiter les lignes utilisées par le démon. Si la carte expose les lignes voulues sur une autre puce, ajoutez une autre règle exacte `KERNEL=="gpiochipN"` et une surcharge `DeviceAllow=` correspondante, plutôt que d'utiliser le joker `gpiochip*`.

Modifiez la configuration et le nom d'hôte du broker, puis effectuez la validation dans l'environnement d'identifiants du service et démarrez le service :

```bash
sudo systemd-run --wait --pipe --uid=mqtt-raspberry-controller \
  --property=LoadCredential=mqtt-password:/etc/mqtt-raspberry-controller/mqtt-password \
  /bin/sh -c 'export MQTT_PASSWORD_FILE="$CREDENTIALS_DIRECTORY/mqtt-password"; exec /usr/local/bin/mqtt-raspberry-controller --config /etc/mqtt-raspberry-controller/config.yaml --check'
sudo systemctl daemon-reload
sudo systemctl enable --now mqtt-raspberry-controller
sudo journalctl -u mqtt-raspberry-controller -f
```

### Bac à sable du service et exceptions de compatibilité

L'unité supprime toutes les capacités, définit `NoNewPrivileges=yes`, n'expose que `/dev/gpiochip0`, rend le système de fichiers hôte accessible en lecture seule, masque les répertoires personnels, isole `/tmp` et les modifications de montage, limite les espaces de noms et les familles d'adresses, et soumet l'ensemble de l'arborescence des processus à des limites de mémoire, de tâches et d'arrêt. `PrivateDevices=yes` n'est volontairement pas utilisé, car il masquerait le véritable périphérique GPIO. Les actions `command` configurées héritent du même bac à sable et s'exécutent toujours en tant que `mqtt-raspberry-controller` ; le service n'élèvera pas silencieusement les privilèges d'une commande configurée.

Gardez les exceptions limitées et locales dans `sudo systemctl edit mqtt-raspberry-controller`. Par exemple, accordez à une action un répertoire inscriptible préparé sans affaiblir le reste du système de fichiers :

```ini
[Service]
ReadWritePaths=/var/lib/mqtt-raspberry-controller/action-output
```

Créez ce répertoire en le faisant appartenir à `mqtt-raspberry-controller`, ou installez les exécutables des actions hors de `/home` (par exemple sous `/usr/local/libexec`). Une commande qui nécessite une autre famille de sockets exige un remplacement explicite tel que `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6 AF_NETLINK`. Ne surchargez `MemoryMax=` ou `TasksMax=` qu'avec une limite mesurée. Pour une opération réellement privilégiée, ne désactivez **pas** `NoNewPrivileges` et n'ajoutez pas de capacités à ce démon ; placez l'unique opération autorisée dans un service ou assistant root distinct et audité séparément, puis n'autorisez que son activation IPC strictement limitée. Après toute surcharge, exécutez `systemd-analyze verify mqtt-raspberry-controller.service` et examinez `systemctl cat mqtt-raspberry-controller`.

Avec systemd 252, `systemd-analyze security --offline=yes packaging/systemd/mqtt-raspberry-controller.service` indique un score d'exposition de **3.2 (`OK`)**. Les scores varient selon les versions de systemd ; GPIO et MQTT nécessitent les exceptions intentionnelles relatives au périphérique, au groupe supplémentaire et au réseau.

### Test rapide root sur Raspberry Pi

`packaging/tests/smoke-raspberry-pi.sh` vérifie l'unité installée, les propriétaires et modes requis de la configuration et du périphérique, l'UID réel du service, les journaux des switches, sorties et lignes de l'invocation systemd actuelle, les états MQTT `online`/état/`offline`, ainsi qu'un véritable basculement GPIO OFF/ON avec restauration de l'état initial. Les identifiants installés restent fournis par le `LoadCredential=` de l'unité, tandis que les identifiants du client de test proviennent uniquement des variables d'environnement `MQTT_RASPBERRY_SMOKE_*` et sont placés dans une configuration cliente Mosquitto temporaire privée. Le nom de sortie explicite doit être le nom `gpio.outputs` référencé par l'état GPIO du switch testé. Chaque opération Mosquitto, y compris le nettoyage après un échec, possède un délai d'expiration ; la restauration attend un acquittement d'état actif correspondant avant que le démon puisse être arrêté. Si la restauration ne peut pas être confirmée, un démon démarré par le test est délibérément laissé en fonctionnement afin de ne pas interrompre les travaux de restauration en attente, et le test échoue avec un avertissement exigeant une intervention manuelle. Le script arrête et redémarre le service canonique ; ne l'exécutez donc que sur une sortie de test sûre :

```bash
export MQTT_RASPBERRY_SMOKE_CONFIRM=YES
export MQTT_RASPBERRY_SMOKE_HOST=broker.example.net
export MQTT_RASPBERRY_SMOKE_PORT=1883
export MQTT_RASPBERRY_SMOKE_BASE_TOPIC=mqtt-raspberry-controller/test-pi
export MQTT_RASPBERRY_SMOKE_SWITCH_ID=relay
export MQTT_RASPBERRY_SMOKE_OUTPUT_NAME=relay
export MQTT_RASPBERRY_SMOKE_GPIO_CHIP=/dev/gpiochip0
export MQTT_RASPBERRY_SMOKE_GPIO_LINE=17
# Optional authentication/TLS:
# export MQTT_RASPBERRY_SMOKE_USERNAME=mqtt-smoke
# read -rsp 'MQTT password: ' MQTT_RASPBERRY_SMOKE_PASSWORD; export MQTT_RASPBERRY_SMOKE_PASSWORD
# export MQTT_RASPBERRY_SMOKE_CAFILE=/etc/ssl/certs/ca-certificates.crt
sudo --preserve-env=MQTT_RASPBERRY_SMOKE_CONFIRM,MQTT_RASPBERRY_SMOKE_HOST,MQTT_RASPBERRY_SMOKE_PORT,MQTT_RASPBERRY_SMOKE_BASE_TOPIC,MQTT_RASPBERRY_SMOKE_SWITCH_ID,MQTT_RASPBERRY_SMOKE_OUTPUT_NAME,MQTT_RASPBERRY_SMOKE_GPIO_CHIP,MQTT_RASPBERRY_SMOKE_GPIO_LINE,MQTT_RASPBERRY_SMOKE_USERNAME,MQTT_RASPBERRY_SMOKE_PASSWORD,MQTT_RASPBERRY_SMOKE_CAFILE \
  ./packaging/tests/smoke-raspberry-pi.sh
```

`make packaging-check` vérifie toujours les modes des fichiers préparés et la validité de l'unité. Lorsqu'il est exécuté en tant que root, il applique et vérifie la propriété numérique des fichiers préparés ; sans root, il vérifie textuellement les commandes d'installation documentées pour le propriétaire et le groupe, car il ne peut pas attribuer les fichiers à `root` ni au groupe sysusers synthétique. Le test rapide Raspberry Pi constitue la vérification de référence du propriétaire installé et du périphérique.

## Validation de l'exemple de configuration

L'exemple suivi par le dépôt peut être vérifié sans ouvrir de périphériques GPIO ni se connecter à MQTT :

```bash
password_file=$(mktemp)
trap 'rm -f "$password_file"' EXIT
chmod 0600 "$password_file"
printf 'development-only-password\n' >"$password_file"
export MQTT_PASSWORD_FILE="$password_file"
./bin/mqtt-raspberry-controller --config ./configs/config.example.yaml --check
```

## Transport MQTT, mTLS et ACL du broker

Les schémas de broker sécurisés (`ssl`, `tls`, `mqtts`, `mqtt+ssl`, `tcps` et `wss`) vérifient le certificat du broker et exigent TLS 1.2 par défaut ; définissez `mqtt.tls.min_version: "1.3"` pour exiger TLS 1.3. Un `ca_file` vide utilise les autorités racines du système. Un fichier d'autorité de certification personnalisé complète le magasin système au lieu de le remplacer. `server_name` ne remplace la vérification du certificat et du DNS que lorsque la connexion s'effectue via une autre adresse ; la vérification ne peut pas être désactivée. Configurez à la fois `cert_file` et un `key_file` à accès restreint pour activer mTLS. Les schémas non chiffrés `tcp`, `mqtt` ou `ws` sont acceptés sans adhésion explicite uniquement pour les adresses de bouclage littérales ou `localhost` ; une connexion distante en clair nécessite `allow_insecure_transport: true`. L'authentification est obligatoire, sauf si `allow_unauthenticated: true` est explicitement défini.

Pour mTLS sous systemd, installez le matériel de l'autorité de certification et du certificat client ainsi que la clé privée, tous appartenant à root, copiez `packaging/systemd/mqtt-raspberry-controller-mtls.conf` dans `/etc/systemd/system/mqtt-raspberry-controller.service.d/mtls.conf`, puis ajoutez ces champs à `mqtt.tls` :

```yaml
ca_file: "${MQTT_CA_FILE}"
cert_file: "${MQTT_CLIENT_CERT_FILE}"
key_file: "${MQTT_CLIENT_KEY_FILE}"
```

La clé client doit avoir pour mode exact `0400`, `0600` ou `0640` ; les identifiants systemd utilisent `0400`, tandis que les fichiers de certificat et d'autorité de certification peuvent être publics. Exécutez `systemctl daemon-reload` et `mqtt-raspberry-controller --check` après toute modification. Le chemin de vérification analyse le mot de passe, l'autorité de certification, le certificat et la clé sans ouvrir le GPIO ni se connecter au broker.

Utilisez un compte Mosquitto dédié et installez `packaging/mosquitto/mqtt-raspberry-controller.acl` comme ACL de l'exemple fourni. Elle ne contient **aucun joker** et n'accorde que les abonnements exacts aux commandes et les publications exactes d'état, de statut, d'actions et de Discovery utilisés par ce YAML. Par exemple :

```conf
password_file /etc/mosquitto/passwd
acl_file /etc/mosquitto/mqtt-raspberry-controller.acl
allow_anonymous false
```

Lors de l'ajout ou du renommage d'une entité, ajoutez son topic de commande exact (`.../set` ou `.../press`), son topic d'état le cas échéant, et son topic Discovery exact. Conservez l'ancienne permission exacte de publication Discovery jusqu'à ce que le démon ait supprimé cette configuration conservée. Lors de l'ajout d'une action `mqtt`, ajoutez le topic de publication exact correspondant. Ne remplacez pas ces entrées par les jokers `#` ou `+`.

## Topics

Avec `base_topic: mqtt-raspberry-controller/example` :

```text
mqtt-raspberry-controller/example/status
mqtt-raspberry-controller/example/relay/set
mqtt-raspberry-controller/example/relay/state
mqtt-raspberry-controller/example/announce/press
mqtt-raspberry-controller/example/contact/state
```

Home Assistant MQTT Discovery est publié sous le préfixe Discovery configuré, généralement `homeassistant`.

## Numérotation GPIO

Les lignes GPIO sont des offsets GPIO Linux, et non les numéros des broches physiques du connecteur Raspberry Pi. La broche physique 7 du Raspberry Pi correspond à BCM GPIO4 ; utilisez donc `line: 4` pour un périphérique connecté à ce GPIO.
