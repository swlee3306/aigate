# 홈서버 OpenStack 현장 조사 (오케스트레이터 VM 3대용)

조사일 2026-11-05, 읽기 전용. 표기: **[확인]** = 실제 명령 출력으로 본 사실, **[추정]** = 출력에서 유추, **[미확인]** = 확인 못 함.

## 1. openstack CLI 실행 방법 [확인]
- 접속: `ssh openstack-host` (192.168.1.121, root, 호스트명 `homeserver`). `/usr/bin/openstack` 설치됨.
- `openrc` 파일은 없다(`/root`에 없음, 셸 환경에 `OS_*` 변수도 없음).
- 인증 정보는 `/etc/openstack/clouds.yaml`의 `exporter-internal` 항목(admin / project admin / Default 도메인). 사용법: `openstack --os-cloud exporter-internal <명령>`. 키 이름: `auth_url, username, password, project_name, user_domain_name, project_domain_name, endpoint_overrides`. 값은 기록하지 않음.
- 엔드포인트는 모두 `http://192.168.1.121:<port>` (keystone 5000, nova 8774, neutron 9696, glance 9292, cinder 8776). 같은 admin 프로젝트 ID: `4c1c2921a66043809a0e6f05c323fa0a`.
- 주의: 이 계정은 admin이며 clouds.yaml은 exporter 용도로 만들어진 것. Task 1에서 재사용할지, 전용 자격 증명을 쓸지 사용자 결정 필요.

## 2. 이미지 [확인]
| 이름 | ID | 크기 | 비고 |
|---|---|---|---|
| **Ubuntu 24.04** | `ae7d9d3c-a56e-4059-b13a-34efacfabb5f` | 625,911,296 B (~597MB) qcow2 | public, active. `hw_disk_bus=scsi`, `virtio-scsi`, `hw_qemu_guest_agent=yes`. k8s-cp-01이 사용 중 |
| Ubuntu 22.04 Vanilla | `66593265-eeb4-42c9-8ac7-486e9550e176` | 691,044,352 B | vm-bastion이 사용 중 |
| cirros / openSUSE Leap 15.6 | (생략) | | 해당 없음 |

결론: 우분투 클라우드 이미지는 **있다**(업로드 불필요). Ubuntu 24.04를 권장. 단 cloud-init 지원(사용자·ssh키 주입)은 기존 VM이 키페어로 부팅된 점에서 **[추정]**으로 동작함. 볼륨 부팅 시 `openstack server create --boot-from-volume 10 --image "Ubuntu 24.04"` 방식이 가능(디스크 버스 scsi 속성이 있으므로 첫 부팅 때 확인 필요 **[미확인]**).

## 3. Flavor [확인]
존재하는 것: m1.tiny(1/0.5G/5G), m1.small(1/2G/20G), m1.medium(2/4G/40G), m1.large(4/8G/80G), m1.xlarge(8/16G/160G), m1.homeserver.{nano,tiny,small,medium,large}, k8s.medium-80g, k8s.medium-3g(2/3G/20G), k8s.medium-3g-root0(2/3G/**0G**), k8s.medium-3g-root10, gpu.gtx660(GPU 패스스루, 사용 금지).

| VM | 제안 사양 | 맞는 기존 flavor | 판단 |
|---|---|---|---|
| orch-vm | 1 vCPU / 2GB / 10GB | `m1.small`(1/2G/20G) 또는 `m1.homeserver.small`(1/2G/20G) | vCPU·RAM은 일치. 디스크는 볼륨 부팅이면 flavor 디스크가 무시되므로 사용 가능 **[추정]**. 정확히 맞는 flavor는 없음 |
| agent-vm | 2 / 4GB / 20GB | `m1.medium`(2/4G/40G), `m1.homeserver.medium` | 위와 동일 |
| test-vm | 4 / 6GB / 30GB | 없음 (`m1.large`는 4/8G/80G) | 새 flavor 필요(예: `aigate.test` 4/6144/0) 또는 m1.large(RAM 낭비) |

권장(설명만, 실행 안 함): disk=0 flavor 3개를 새로 만들면 볼륨 크기만 지정해 정확히 맞출 수 있다 — `aigate.orch 1/2048/0`, `aigate.agent 2/4096/0`, `aigate.test 4/6144/0`. flavor 생성은 admin 권한이 필요하며 Task 1 시작 전 사용자 승인 필요. 기존 `k8s.medium-3g-root0`은 disk=0 선례 **[확인]**.

## 4. 네트워크 [확인]
| 네트워크 | 서브넷 | 비고 |
|---|---|---|
| `mgmt-net` (vxlan) | mgmt-subnet 10.10.0.0/24, gw 10.10.0.1, DNS 8.8.8.8, pool .2-.254 | 기존 VM 2대가 사용. router-main에 연결, SNAT 켜짐 |
| `api-net` | api-subnet 10.20.0.0/24 | router-main에 연결, VM 없음 |
| `internal-net` | 10.30.0.0/24 | internal-router(외부 게이트웨이 없음) |
| `test-internal-net` | test-int-subnet 10.60.0.0/24 | internal-router(외부 게이트웨이 없음) |
| `external-net` (flat) | external-subnet 192.168.1.0/24, gw 192.168.1.1, pool 192.168.1.200-254, DHCP 꺼짐 | 가정 LAN과 동일 대역 |

- 라우터: `router-main`(외부 GW 192.168.1.253, SNAT) = mgmt-net+api-net, `external-router`(외부 GW 192.168.1.230, 인터페이스 없음), `internal-router`.
- Floating IP 가능: 예. external-net 풀 200-254에서 사용 중인 것은 .204(vm-bastion), .251(k8s-cp-01), 게이트웨이 .230, .253. 쿼터 floating-ips 50.
- 보안 그룹: Neutron OVS `firewall_driver=noop`로 **미적용** [확인]. 따라서 3개 VM의 접근 제한은 VM 내부 nftables가 전부 책임진다.

### 기존 VM 접근 방식 (Mac/Tailscale에서)
- LAN: VM은 floating IP(192.168.1.x)로 LAN에서 직접 접근. 예: bastion `192.168.1.204`, `~/.ssh/config` `bastion`이 이 주소 사용 [확인].
- 원격(Tailscale): **vm-bastion VM 자체가 Tailscale에 가입**돼 있다 (`100.64.0.23`, online) [확인]. 홈서버도 가입(`100.64.0.22`, 호스트명 homeserver-openstack). 홈서버는 서브넷 라우터가 **아니다**(`AdvertiseRoutes` 없음, 광고 라우트 없음) [확인]. 서브넷 라우터는 `bon-openstack`(100.64.0.10)이 10.10.11.0/24, 10.10.12.0/24, 192.168.0.0/24만 광고 — 우리 대역과 무관.
- 결론: 새 VM이 Tailscale로 닿으려면 VM 내부에 tailscale을 설치해야 한다(bastion과 같은 방식).

## 5. Cinder / 스토리지 [확인]
- 볼륨 타입: `__DEFAULT__` 하나뿐. 백엔드 LVM(iSCSI, tgtadm), VG `cinder_volumes`.
- 씬 풀 `cinder_volumes-pool` 330.84G, 실사용 1.93%. VG 잔여 17.24G(씬 풀 외 영역). 볼륨 1개: `k8s-migration-backup-30g` 30GB(vm-bastion에 `/dev/vdb`로 연결, ID `09164d88-...`).
- 우리 볼륨 합계 10+20+30=60GB: 풀 용량상 충분. 씬 프로비저닝이므로 총 할당 한도는 쿼터(300GB)가 결정.
- 호스트 루트 `/` 98G 중 54G 여유. 호스트 8코어, RAM 31GB(조사 시점 free 12G, available 17G).
- 키페어(이름만): `blog-evidence-terraform-20260916`, `brain-memory-key`, `k8s-cp-key`, `mykey`. 새 VM용 키페어는 없다 — 기존 `mykey`(vm-bastion이 사용, Mac의 id_rsa와 같은지는 **[미확인]**)를 재사용하거나 전용 키를 사용자가 등록해야 한다.

## 6. 쿼터와 사용량 [확인]
| 항목 | 한도 | 사용 | 우리 3대 추가 후 |
|---|---|---|---|
| instances | 10 | 2 | 5 |
| cores | 20 | 3 | 10 (1+2+4=7 추가) |
| ram | 24576MB | 6144 | 18432 (12288 추가) |
| volumes | 20 | 1 | 4 |
| gigabytes | 300 | 30 | 90 |
| floating-ips | 50 | 0 (limits 기준; 실제 FIP 2개 존재) | |
쿼터는 충분. 단 호스트 물리 RAM(31GB, 이미 13GB 사용)이 실질 한계: 12GB 추가해도 여유가 있으나 gpu 작업 등과 겹치면 빠듯할 수 있음 **[추정]**. CPU 오버커밋은 기본값 가정.

## 7. 기존 VM [확인]
| 이름 | libvirt | flavor | 네트워크/IP | 이미지 | 키 | 용도(추정) |
|---|---|---|---|---|---|---|
| k8s-cp-01 | instance-0000001c? | m1.medium(2/4G) | mgmt-net 10.10.0.68, FIP 192.168.1.251 | Ubuntu 24.04 | k8s-cp-key | k3s 컨트롤 플레인 (sg `k8s-cp-sg`) |
| vm-bastion | instance-0000006b? | m1.small(1/2G) | mgmt-net 10.10.0.31, FIP 192.168.1.204, Tailscale 100.64.0.23 | Ubuntu 22.04 | mykey | 배스천, 볼륨 30GB 연결 |
(OpenStack 이름과 libvirt 인스턴스 이름의 짝은 확인하지 않음 — `virsh list`는 `instance-0000001c`, `instance-0000006b` 두 개 running.)

## 8. nftables에서 "LAN으로 보고 deny"할 대역
- 가정 LAN: **192.168.1.0/24** (게이트웨이 192.168.1.1)
  - 홈서버 호스트: 192.168.1.121 (Tailscale 100.64.0.22)
  - gpu-server: 192.168.1.129 (ssh config; Tailscale 100.104.116.73) — LAN IP는 ssh config 기준 **[추정]**, 직접 접속해 확인하지 않음
  - Mac mini: 192.168.1.128 (ssh config; Tailscale 100.64.0.25) — 동일하게 **[추정]**
  - 이 조사를 실행한 MacBook Pro: LAN 192.168.1.124 [확인], Tailscale 100.64.0.24
  - bastion floating 192.168.1.204, k8s-cp 192.168.1.251, 라우터 .253/.230
  - 그 외 192.168.1.x 전체(공유기, 가정 기기)
- 홈서버 OpenStack 내부 네트워크: 10.10.0.0/24(mgmt), 10.20.0.0/24, 10.30.0.0/24, 10.60.0.0/24 — VM 간 이동을 막으려면 이들도 deny 대상. 홈서버 libvirt 기본망 192.168.122.0/24도 포함 권장.
- Tailscale: **100.64.0.0/10** 전체 deny(그 안에서 사용자 접속용 예외만 별도 allow).
- 다른 서브넷 라우터 광고 대역(참고): 10.10.11.0/24, 10.10.12.0/24, 192.168.0.0/24 — Tailscale 경유로 도달 가능하므로 deny 권장. 또 RFC1918 전체(10/8, 172.16/12, 192.168/16)와 링크로컬 169.254.0.0/16(메타데이터 169.254.169.254는 cloud-init 후에는 불필요)을 기본 deny하고 필요한 목적지만 allow하는 구성을 추천.
- 허용해야 할 것(결정 필요): DNS(10.10.0.1 또는 8.8.8.8), 외부 LLM API 도달에 필요한 인터넷(443). 게이트웨이 10.10.0.1은 인터넷 경로이므로 순수 deny 불가.

## 9. 제안 이름·IP·서브넷
- 서브넷: 기존 `mgmt-net`(10.10.0.0/24)은 이미 다른 VM이 쓰므로 **전용 네트워크 신설을 권장** — 예: `aigate-net` / `aigate-subnet` 10.70.0.0/24(미사용 확인: 10.10/20/30/60과 겹치지 않음), router-main에 연결해 SNAT 인터넷 확보. 네트워크·서브넷 생성은 Task 1에서 사용자 승인 후 수행. 대안: mgmt-net 재사용(간단하지만 k8s-cp·bastion과 같은 L2).
- 고정 IP 제안: orch-vm 10.70.0.11, agent-vm 10.70.0.12, test-vm 10.70.0.13 (게이트웨이 10.70.0.1).
- 사용자 접속: orch-vm에 floating IP 1개(예: 192.168.1.2xx, 풀 200-254 중 미사용) 부여해 LAN에서 SSH, 원격은 Tailscale.
- 이름: `orch-vm`, `agent-vm`, `test-vm`. 볼륨: `orch-vm-root`(10G), `agent-vm-root`(20G), `test-vm-root`(30G), 모두 `__DEFAULT__`.
- 보안그룹은 미적용이므로 `default`를 붙여도 효과 없음. 모든 제어는 VM 내부 nftables.

## 10. Tailscale 합류 방법 (설명만)
1. 사용자가 Tailscale 관리 콘솔에서 **인증 키(auth key)** 생성(가급적 일회용·태그 지정·단기 만료, 임시(ephemeral) 선택 가능). 키는 대화·저장소에 남기지 않는다.
2. orch-vm 내부에서 tailscale 설치 후 `tailscale up --authkey=<키> --hostname=orch-vm` 실행(키는 사용자가 직접 입력하거나 cloud-init 일회성 파일로).
3. 합류 후 Tailscale IP(100.x.y.z)는 **할당 전에는 알 수 없음** [미확인]. 완료 후 `tailscale ip -4`로 확인. 우리는 이 IP로 Mac에서 접속.
4. 주의: VM 내부 nftables가 100.64.0.0/10 아웃바운드를 deny하더라도, 사용자 접속을 위해 tailscale0 인터페이스의 인바운드 SSH/오케스트레이터 포트는 허용하고, tailscaled의 컨트롤 서버 연결(인터넷 443/UDP 41641 등)은 허용해야 한다. agent-vm·test-vm은 Tailscale 미가입이 원칙에 더 부합(필요 시 사용자 결정).

## 11. Task 1 열린 질문 / 블로커
1. **flavor**: 정확한 사양이 없음 → 새 flavor 3개 생성 승인 필요(아니면 m1.small/m1.medium/m1.large 사용; test-vm은 RAM 8GB로 낭비).
2. **네트워크**: 전용 `aigate-net`(10.70.0.0/24) 신설 vs mgmt-net 재사용 — 사용자 결정. 보안그룹이 미적용이라 mgmt-net 공유는 k8s-cp/bastion과 격리가 안 됨.
3. **키페어**: 새 VM용 키 지정 필요(`mykey` 재사용 여부 및 공개키 출처).
4. **Floating IP**: orch-vm용 1개 사용 가능한지 사용자 확인(LAN 노출).
5. **Tailscale auth key**: 사용자가 생성해서 제공해야 한다(대기 중).
6. **LAN IP 미검증**: gpu-server·Mac mini의 LAN IP는 ssh config 값. 로그인해 확인하지 않음.
7. **자격 증명**: `exporter-internal` 계정(admin)을 Task 1에서 써도 되는지 확인. 최소 권한 계정을 별도로 만들지 결정.
8. **호스트 RAM 여유**: 12GB 추가 시 호스트 가용 ~17GB → ~5GB. GPU VM 작업과 동시 실행 시 위험. 오버커밋 정책 **[미확인]**(nova.conf 미조사).
9. 이미지 `hw_disk_bus=scsi` 속성 하에서의 볼륨 부팅은 첫 시도로 확인 필요.
10. 막힌 것(이미지 없음·경로 없음·쿼터 부족)은 **없다**.

## 12. 읽기 전용 검증
`server list -f value -c ID`와 `volume list -f value -c ID`의 조사 전·후 출력이 동일함(서버 2개 `1a5de248-...`, `bad2afe3-...`, 볼륨 1개 `09164d88-...`). 자세한 증거는 보고서 참조.
