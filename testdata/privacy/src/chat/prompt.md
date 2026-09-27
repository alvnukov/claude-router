Привет. Помоги разобраться с сетью в новом офисе ⟦org:Ромашки⟧ (⟦address:Цветогорск, Садовая ул., 15⟧).
Ноутбук ⟦person:Петрова⟧ не получает адрес по DHCP, а у ⟦person:Смирновой⟧ всё работает.

Вывод `ip a` с ноутбука:

```
2: wlp2s0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP
    link/ether ⟦mac:00:00:5e:00:53:91⟧ brd ⟦keep:ff:ff:ff:ff:ff:ff⟧
    inet ⟦cidr4:169.254.12.7/16⟧ brd ⟦keep:169.254.255.255⟧ scope link wlp2s0
    inet6 ⟦cidr6:fe80::200:5eff:fe00:5391/64⟧ scope link
```

Трассировка до ⟦host:intranet.corp.romashka.example⟧:

```
traceroute to ⟦host:intranet.corp.romashka.example⟧ (⟦ipv4:10.113.20.5⟧), 30 hops max, 60 byte packets
 1  ⟦host:ap-3f.office.romashka.example⟧ (⟦ipv4:192.168.77.1⟧)  1.804 ms
 2  ⟦ipv4:172.20.0.1⟧  3.112 ms
 3  * * *
```

DHCP-сервер у нас ⟦ipv4:192.168.77.2⟧, пул ⟦ipv4:192.168.77.100⟧–⟦ipv4:192.168.77.199⟧, аренда 8 ч.
Если нужно, напиши ⟦person:Ивану Петрову⟧ на ⟦email:ivan.petrov@romashka.example⟧ или позвони ⟦phone:+7 (000) 123-45-67⟧.
Договор на канал у нас с ⟦org:ООО «Василёк Телеком»⟧, ИНН ⟦keep:0000000000⟧.
