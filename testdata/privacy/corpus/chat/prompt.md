Привет. Помоги разобраться с сетью в новом офисе Ромашки (Цветогорск, Садовая ул., 15).
Ноутбук Петрова не получает адрес по DHCP, а у Смирновой всё работает.

Вывод `ip a` с ноутбука:

```
2: wlp2s0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500 qdisc noqueue state UP
    link/ether 00:00:5e:00:53:91 brd ff:ff:ff:ff:ff:ff
    inet 169.254.12.7/16 brd 169.254.255.255 scope link wlp2s0
    inet6 fe80::200:5eff:fe00:5391/64 scope link
```

Трассировка до intranet.corp.romashka.example:

```
traceroute to intranet.corp.romashka.example (10.113.20.5), 30 hops max, 60 byte packets
 1  ap-3f.office.romashka.example (192.168.77.1)  1.804 ms
 2  172.20.0.1  3.112 ms
 3  * * *
```

DHCP-сервер у нас 192.168.77.2, пул 192.168.77.100–192.168.77.199, аренда 8 ч.
Если нужно, напиши Ивану Петрову на ivan.petrov@romashka.example или позвони +7 (000) 123-45-67.
Договор на канал у нас с ООО «Василёк Телеком», ИНН 0000000000.
