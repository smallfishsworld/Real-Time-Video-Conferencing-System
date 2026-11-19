import asyncio
import threading
import socket
import pickle
from util import *


class ConferenceServer(threading.Thread):
    def __init__(self, conference_id, creator):
        super().__init__()
        self.conference_id = conference_id  
        self.skt = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.skt.bind((SERVER_IP,conference_id+10000))
        self.skt.listen(20)

        self.data_serve_ports = {}
        self.data_types = ['screen', 'camera', 'audio']  # example data types in a video conference
       
        self.client_conns = []   
        self.mode = 'Client-Server'  # or 'P2P' if you want to support peer-to-peer conference mode
        self.running = True
        
        self.conns = dict()
        self.host = creator
        print("#conf server create:"+str(conference_id)+" "+creator)
        
        self.clients_info = {}  
        
        
        

    def handle_data(self):
        """
        running task: receive sharing stream data from a client and decide how to forward them to the rest clients
        """
        
        self.sock_transfer = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock_transfer.bind((SERVER_IP,(self.conference_id*100)+10000))
        
        while self.running:
            data, address = self.sock_transfer.recvfrom(100000) 
            
            for nid, cli_addr in self.clients_info.copy().items():
                
                if cli_addr == address[0]:
                    continue

                snd_conn = (cli_addr, 20000+(nid*100))
                self.sock_transfer.sendto(data, snd_conn)
        

    def handle_client(self, cli_conn):
        """
        running task: handle the in-meeting requests or messages from clients
        """
        while self.running:
            try:
                message = cli_conn.recv(1024).decode().strip()
                if not message:
                    break
                
                print("$conf server receive message:"+message)
                
                cmd=message.split(":")
                
                print("SERVERID",self.conference_id)
                print("HAN",message)
                print("S",self.client_conns)
                print("T",self.clients_info)
                
                if len(cmd)==2:
                    if cmd[0] == "quit":
                        for cli in self.client_conns.copy():
                            cli.send(message.rjust(10).encode())
                        
                        self.client_conns.remove(cli_conn)
                        ID=int(cmd[1])
                        del self.clients_info[ID]
                        
                        cli_conn.close()
                        if(len(self.client_conns)==0):
                            self.terminate_conference()
                            
                    elif cmd[0] == "send":
                        for cli in self.client_conns.copy():
                            cli.send(message.rjust(10).encode())
                    # elif cmd[0] == "join":
                            
                elif message == "cancel":
                    for cli in self.client_conns.copy():
                        cli.send("exit".rjust(10).encode())
                        cli.close()
                    self.terminate_conference()
                else:
                    for cli in self.client_conns.copy():
                        cli.send(message.rjust(10).encode())
            except:
                break

    async def log(self):
        while self.running:
            print('Something about server status')
            await asyncio.sleep(LOG_INTERVAL)

    def terminate_conference(self):
        self.running=False
        self.skt.close()
        print("conference"+str(self.conference_id)+" has been terminated")

 

    def run(self):
        """
        start the ConferenceServer and necessary running tasks to handle clients in this conference
        """
        print("start conf server No."+str(self.conference_id))
        
        thd21=threading.Thread(target=self.handle_data)
        thd21.start()

        while True:
            try:
                cli_conn, address = self.skt.accept()
                
                print("check ",address[1])
                
                
                se="joinlist:"
                for nid, cli_addr in self.clients_info.copy().items():
                    se=se+str(nid)+"#"+str(cli_addr)+":"
                    
                cli_conn.send(se.encode())


                ID = address[1]-20000
                
                for cli in self.client_conns.copy():
                    se="join:"+str(ID)+"#"+str(address[0])
                    cli.send(se.rjust(10).encode())
                            
                
                
                print("ID",ID)
                
                self.clients_info[ID]=address[0]
                self.client_conns.append(cli_conn)
                
                
                thread = threading.Thread(target=self.handle_client, args=(cli_conn,))
                
                
                thread.start()
                
            except:
                break

       




class MainServer:
    def __init__(self, server_ip, main_port):
        self.server_ip = server_ip
        self.server_port = main_port
        self.main_server = (server_ip, main_port)
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.sock.bind(self.main_server)
        self.sock.listen(10)

        self.conference_confs = set()
        self.conference_id_dict = dict()  
        print('start')

    def broadcast(self, message):
        print('broadcast: ' + message)
        for conference in self.conference_confs:
            for key, conn in conference.conns.items():
                conn.send(message.encode())

    def handle_creat_conference(self, conn):
        """
        create conference: create and start the corresponding ConferenceServer, and reply necessary info to client
        """
        name = conn.recv(10).decode().strip()
        conference_id = conn.recv(10).decode().strip()
        if self.conference_id_dict.get(conference_id, 'no') == 'no':



            confthd = ConferenceServer(int(conference_id),name)
            
            self.conference_id_dict[conference_id] = confthd
            self.conference_confs.add(confthd)

            confthd.start()


            return SUCCESS('create:' + conference_id)
        else:
            return FAILURE('create:' + conference_id)

    def handle_join_conference(self, conn):
        """
        join conference: search corresponding conference_info and ConferenceServer, and reply necessary info to client
        """
        name = conn.recv(10).decode().strip()
        conference_id = conn.recv(10).decode().strip()
        if self.conference_id_dict.get(conference_id, 'no') != 'no':
            conference = self.conference_id_dict[conference_id]
            conference.conns[name] = conn

            if conference.running == False:
                return FAILURE('join:' + conference_id)
            else:
                return SUCCESS('join:' + conference_id)
            
        else:
            return FAILURE('join:' + conference_id)

    def handle_quit_conference(self, conn):
        """
        quit conference (in-meeting request & or no need to request)
        """
        name = conn.recv(10).decode().strip()
        conference_id = conn.recv(10).decode().strip()

        if self.conference_id_dict.get(conference_id, 'no') != 'no':
            conference = self.conference_id_dict[conference_id]
            conference.conns.pop(name, None)
            return 'succeed in quit ' + conference_id
        else:
            return 'fail in quit ' + conference_id

    def handle_cancel_conference(self, conn):
        """
        cancel conference (in-meeting request, a ConferenceServer should be closed by the MainServer)
        """
        name = conn.recv(10).decode().strip()
        conference_id = conn.recv(10).decode().strip()
        if self.conference_id_dict.get(conference_id, 'no') != 'no':
            conference = self.conference_id_dict[conference_id]
            if conference.host == name:
                self.conference_confs.remove(self.conference_id_dict[conference_id])
                self.conference_id_dict.pop(conference_id, None)
                return 'succeed in canceling ' + conference_id
            
        return 'fail in canceling ' + conference_id

    def toString(self):
        re = ''
        for key, value in self.conference_id_dict.items():
            re += key + ' '
        return re.strip()

    def request_handler(self, conn, address):
        """
        running task: handle out-meeting (or also in-meeting) requests from clients
        """
        while True:
            message = conn.recv(1024).decode().strip()
            if not message:
                break
            print(f"Received: {message}")

            response = 'None'

            if message == 'create':
                response = self.handle_creat_conference(conn)
                conn.send(response.encode())
            elif message == 'join':
                response = self.handle_join_conference(conn)
                conn.send(response.encode())
            elif message == 'quit':
                response = self.handle_quit_conference(conn)
                conn.send(response.encode())
            elif message == 'cancel':
                response = self.handle_cancel_conference(conn)
                conn.send(response.encode())

            print(response)
            if response == 'quit':
                break

        conn.close()

    def run(self):
        """
        start MainServer
        """
        while True:
            conn, address = self.sock.accept()
            thread = threading.Thread(target=self.request_handler, args=(conn, address))
            thread.start()


if __name__ == '__main__':
    mainServer = MainServer(SERVER_IP, MAIN_SERVER_PORT)
    mainServer.run()
    